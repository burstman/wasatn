// WasaTN front-end glue.
//
// Three jobs:
//   1. Attach the CSRF token to every non-GET HTMX request, so forms never need
//      to wire it up individually.
//   2. Confirm destructive actions.
//   3. Drive Meta Embedded Signup on the connections page: ask the server for a
//      state value, open the Facebook Login dialog, then hand the authorization
//      code back to our own endpoint.
(function () {
  "use strict";

  // Facebook's SDK is loaded lazily and only on the connections page. Bundling it
  // in would cost every visitor a third-party script they do not need.
  var SDK_SRC = "https://connect.facebook.net/en_US/sdk.js";
  var sdkPromise = null;

  function csrfToken() {
    var meta = document.querySelector('meta[name="csrf-token"]');
    return meta ? meta.getAttribute("content") : "";
  }

  document.body.addEventListener("htmx:configRequest", function (event) {
    var verb = event.detail.verb || "get";
    if (verb === "get" || verb === "head") {
      return;
    }
    var token = csrfToken();
    if (token) {
      event.detail.headers["X-CSRF-Token"] = token;
    }
  });

  // Give every destructive action a confirmation without inline handlers.
  document.body.addEventListener("htmx:beforeRequest", function (event) {
    var elt = event.detail.elt;
    if (!elt || !elt.dataset || !elt.dataset.confirm) {
      return;
    }
    if (!window.confirm(elt.dataset.confirm)) {
      event.preventDefault();
    }
  });

  // Alpine owns UI-only state; make sure HTMX swaps are re-scanned by Alpine so
  // newly inserted x-data components initialise.
  document.body.addEventListener("htmx:afterSwap", function (event) {
    if (window.Alpine) {
      window.Alpine.initTree(event.detail.target);
    }
  });

  // --- Embedded Signup -----------------------------------------------------

  // SDK_TIMEOUT_MS bounds how long the Facebook script may take. Without a
  // bound a blocked or hung script leaves the promise pending forever, the
  // button disabled and the user staring at a dead page.
  var SDK_TIMEOUT_MS = 12000;
  var statePromise = null;
  var stateValue = "";

  function showSignupError(button, message) {
    var box = document.getElementById("signup-error");
    var text = document.getElementById("signup-error-message");
    if (box && text) {
      text.textContent = message;
      box.classList.remove("hidden");
    }
    if (button) {
      button.disabled = false;
      button.textContent = "Connect WhatsApp";
    }
  }

  function clearSignupError() {
    var box = document.getElementById("signup-error");
    if (box) {
      box.classList.add("hidden");
    }
  }

  // loadSdk resolves with window.FB once the SDK is initialised.
  //
  // A failed attempt clears sdkPromise so a retry injects the script again
  // rather than reusing a rejected promise forever.
  function loadSdk(version) {
    if (sdkPromise) {
      return sdkPromise;
    }
    sdkPromise = new Promise(function (resolve, reject) {
      var script = null;
      var timer = null;
      var settled = false;

      function cleanup() {
        if (timer) {
          clearTimeout(timer);
        }
        if (script) {
          script.onload = null;
          script.onerror = null;
        }
        delete window.fbAsyncInit;
      }

      function fail(message) {
        if (settled) {
          return;
        }
        settled = true;
        sdkPromise = null;
        cleanup();
        reject(new Error(message));
      }

      function init() {
        try {
          window.FB.init({ xfbml: false, version: version });
        } catch (err) {
          fail(
            "Facebook's sign-up did not start (" +
              (err && err.message ? err.message : "unknown error") +
              "). Reload the page and try again."
          );
          return;
        }
        if (settled || !window.FB) {
          return;
        }
        settled = true;
        cleanup();
        resolve(window.FB);
      }

      if (window.FB) {
        init();
        return;
      }

      timer = setTimeout(function () {
        fail("Facebook's sign-up took too long to load. Check your connection and try again.");
      }, SDK_TIMEOUT_MS);

      window.fbAsyncInit = init;

      script = document.createElement("script");
      script.src = SDK_SRC;
      script.async = true;
      script.crossOrigin = "anonymous";
      script.onerror = function () {
        fail(
          "Facebook's login script was blocked and could not be loaded. " +
            "An ad blocker, privacy extension or network filter is the usual cause: " +
            "allowlist connect.facebook.net, or try a private window with extensions off."
        );
      };
      document.head.appendChild(script);
    });
    return sdkPromise;
  }

  // requestState asks our own server for the state value that proves the signup
  // round trip came back from Meta. It is fetched per attempt rather than cached
  // forever, because the server clears it once it is used.
  function requestState(button) {
    return fetch(button.dataset.stateUrl, {
      method: "POST",
      headers: {
        "X-CSRF-Token": csrfToken(),
        "X-Requested-With": "XMLHttpRequest",
      },
      credentials: "same-origin",
    })
      .then(function (response) {
        if (!response.ok) {
          throw new Error("Could not start the sign-up. Reload the page and try again.");
        }
        return response.json();
      })
      .then(function (body) {
        if (!body || !body.state) {
          throw new Error("Could not start the sign-up. Reload the page and try again.");
        }
        return body.state;
      });
  }

  function postCode(button, code, state) {
    return fetch(button.dataset.callbackUrl, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-CSRF-Token": csrfToken(),
      },
      credentials: "same-origin",
      body: JSON.stringify({ code: code, state: state }),
    });
  }

  function describeFailure(response) {
    return response.text().then(function (body) {
      // The error page is HTML; strip the tags rather than dumping markup into
      // an alert box.
      var text = body.replace(/<[^>]*>/g, " ").replace(/\s+/g, " ").trim();
      return text || "Meta could not finish the sign-up. Please try again.";
    });
  }

  // warmUp fetches everything the click needs: the state value and the SDK.
  //
  // This has to happen *before* the click, not during it. FB.login opens the
  // Facebook dialog in a popup window, and browsers only permit that while the
  // user gesture is still active. Awaiting a fetch first loses the gesture, and
  // Firefox then blocks the popup without saying anything at all.
  function warmUp(button) {
    if (!statePromise) {
      statePromise = requestState(button)
        .then(function (state) {
          stateValue = state;
          return state;
        })
        .catch(function (err) {
          statePromise = null;
          throw err;
        });
      // The preload is an optimisation; a failure is reported on the next click.
      statePromise.catch(function () {});
    }
    if (!sdkPromise) {
      loadSdk(button.dataset.version).catch(function () {});
    }
  }

  // Facebook opens its dialog with window.open. When a blocker stops that, the
  // SDK never calls back and the page simply sits there, so keep the handle if
  // we ever get one (it also lets a second click re-focus the dialog) and treat
  // a refused window as a failure the user can act on.
  function watchPopups(onOpened, onBlocked) {
    var original = window.open;
    window.open = function () {
      var opened = null;
      try {
        opened = original.apply(window, arguments);
      } catch (err) {
        opened = null;
      }
      if (opened) {
        onOpened(opened);
      } else {
        onBlocked();
      }
      return opened;
    };
    setTimeout(function () {
      window.open = original;
    }, POPUP_WATCH_MS);
  }

  var POPUP_WATCH_MS = 10000;

  // Facebook can take a while when it asks the user to pick a business, but a
  // dialog that never answers at all is a blocked popup, so this is a backstop
  // rather than a timeout on the user.
  var DIALOG_TIMEOUT_MS = 45000;

  var signupPopup = null;
  var dialogPending = false;

  function openDialog(button) {
    var state = stateValue;
    var settled = false;
    var watchdog = null;

    dialogPending = true;
    button.textContent = "Opening Facebook…";
    clearSignupError();

    // The state is single use, so a stalled or failed attempt must not leave it
    // behind for the next click to reuse.
    function abandon(message) {
      if (settled) {
        return;
      }
      settled = true;
      dialogPending = false;
      signupPopup = null;
      if (watchdog) {
        clearTimeout(watchdog);
      }
      statePromise = null;
      stateValue = "";
      showSignupError(button, message);
    }

    watchPopups(
      function (handle) {
        signupPopup = handle;
      },
      function () {
        abandon(
          "Your browser blocked the Facebook sign-up window. Allow popups for this site " +
            "(the icon on the left of the address bar) and try again."
        );
      }
    );

    watchdog = setTimeout(function () {
      abandon(
        "Facebook did not finish the sign-up. If no window opened at all, your browser is " +
          "blocking popups for this site — allow them and try again."
      );
    }, DIALOG_TIMEOUT_MS);


    try {
      window.FB.login(function (authResponse) {
        settled = true;
        dialogPending = false;
        if (watchdog) {
          clearTimeout(watchdog);
        }
        if (!authResponse || authResponse.status !== "connected") {
          var reason =
            authResponse && authResponse.error_message
              ? authResponse.error_message
              : "The sign-up was cancelled.";
          showSignupError(button, reason);
          return;
        }
        if (!authResponse.code) {
          showSignupError(
            button,
            "Facebook did not return an authorization code. Check that your app allows response_type=code."
          );
          return;
        }
        button.textContent = "Connecting…";
        postCode(button, authResponse.code, state)
          .then(function (response) {
            if (!response.ok) {
              return describeFailure(response).then(function (message) {
                showSignupError(button, message);
              });
            }
            // The server stored the connection and set a flash message, so a
            // reload is enough to show the result.
            window.location.reload();
          })
          .catch(function () {
            showSignupError(button, "Could not reach WasaTN. Check your connection and try again.");
          });
      }, {
        // config_id selects the Embedded Signup configuration and
        // response_type=code returns an authorization code instead of a cookie.
        // No scope is requested: the pages_* scopes were deprecated by Graph v19
        // and a new app cannot obtain them without App Review.
        config_id: button.dataset.configId,
        response_type: "code",
        state: state,
      });
    } catch (err) {
      abandon(
        "The Facebook dialog could not be opened (" +
          (err && err.message ? err.message : "unknown error") +
          "). Allow popups for this site and try again."
      );
    }
  }

  function startSignup(button) {
    if (button.disabled) {
      return;
    }
    clearSignupError();

    // Everything must already be resolved: FB.login has to run inside the click
    // itself so the popup counts as user-initiated.
    if (!window.FB || !stateValue) {
      warmUp(button);
      button.disabled = true;
      button.textContent = "Loading Facebook…";
      Promise.all([loadSdk(button.dataset.version), requestState(button)])
        .then(function (results) {
          stateValue = results[1];
          button.disabled = false;
          button.textContent = "Connect WhatsApp";
          showSignupError(button, "Facebook is ready now. Click Connect WhatsApp again.");
        })
        .catch(function (err) {
          showSignupError(button, err.message || "Could not start the sign-up.");
        });
      return;
    }

    // A dialog we already opened is somewhere else on the screen, quite
    // possibly behind this tab, so a second click brings it back instead of
    // starting a competing flow.
    if (dialogPending && signupPopup && !signupPopup.closed) {
      signupPopup.focus();
      return;
    }

    openDialog(button);
  }

  function findConnectButton() {
    return document.getElementById("connect-whatsapp");
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest && event.target.closest("#connect-whatsapp");
    if (!button) {
      return;
    }
    event.preventDefault();
    startSignup(button);
  });

  // Warm up as soon as the button exists, and again on the first sign the user
  // shows of interest, so the click is almost always the only thing left.
  function prepare() {
    var button = findConnectButton();
    if (!button) {
      return;
    }
    warmUp(button);
    button.addEventListener("pointerenter", function () {
      warmUp(button);
    });
    button.addEventListener("focus", function () {
      warmUp(button);
    });
  }

  prepare();
  document.addEventListener("htmx:afterSwap", prepare);
})();
