// WasaTN front-end glue.
//
// Three jobs:
//   1. Attach the CSRF token to every non-GET HTMX request, so forms never need
//      to wire it up individually.
//   2. Confirm destructive actions.
//   3. Drive Meta Embedded Signup on the connections page: ask the server for a
//      state value, run Facebook's Embedded Signup dialog through its SDK, then
//      hand the authorization code back to our own endpoint.
(function () {
  "use strict";

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
  //
  // Embedded Signup is launched by Facebook's JavaScript SDK, not by a URL we
  // assemble. The dialog sends the customer back to the window that spawned it and
  // hands that window the exchangeable code, so a dialog opened by hand has
  // nothing to talk to: it closes itself a couple of seconds in. FB.login is the
  // only supported way in.
  //
  // Three details in the flow are not optional. FB.init needs appId, and without it
  // the SDK never finishes initialising and FB.login answers "FB.login() called
  // before FB.init()". FB.login has to run in the click handler itself, because it
  // opens the dialog with window.open and Firefox refuses a window opened from
  // anything but a real user gesture. And the state value the server hands out is
  // fetched up front, so waiting for it never delays the click.
  //
  // The code is valid for 30 seconds, so it goes straight back to our endpoint,
  // which exchanges it and redirects; Meta never redirects this browser itself.
  //
  // The SDK is loaded on the connections page only, since every other visitor has
  // no use for a third-party script.

  var SDK_SRC = "https://connect.facebook.net/en_US/sdk.js";
  var sdkReady = null;
  var running = false;
  var watchdog = 0;
  var statePromise = null;

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

  // loadSDK puts Facebook's script on the page and resolves once FB.init is done.
  //
  // fbAsyncInit is the SDK's own readiness hook: the script calls it when it is
  // loaded, which is the only signal that FB.init is safe to call.
  function loadSDK(button) {
    if (sdkReady) {
      return sdkReady;
    }
    sdkReady = new Promise(function (resolve, reject) {
      window.fbAsyncInit = function () {
        try {
          window.FB.init({
            appId: button.dataset.appId,
            autoLogAppEvents: true,
            xfbml: true,
            version: button.dataset.version,
          });
          resolve();
        } catch (err) {
          reject(new Error("Facebook could not start (" + err.message + "). Reload the page and try again."));
        }
      };

      if (window.FB && window.FB.init) {
        window.fbAsyncInit();
        return;
      }

      var script = document.createElement("script");
      script.async = true;
      script.defer = true;
      script.crossOrigin = "anonymous";
      script.src = SDK_SRC;
      script.onerror = function () {
        reject(new Error("Facebook's script did not load. Check your connection or any ad blocker, then try again."));
      };
      document.head.appendChild(script);
    });
    sdkReady.catch(function () {
      sdkReady = null;
    });
    return sdkReady;
  }

  // requestState asks our own server for the state value that proves the signup
  // round trip came back from Meta. It is fetched before the click rather than in
  // it, so that waiting for it cannot cost us the user gesture. The server clears
  // it once it has been used.
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

  function warmUp(button) {
    if (statePromise) {
      return statePromise;
    }
    statePromise = requestState(button).catch(function (err) {
      statePromise = null;
      throw err;
    });
    statePromise.catch(function () {});
    return statePromise;
  }

  // Embedded Signup reports what happened to the flow as a message from Facebook:
  // the assets it issued on success, or the screen the customer gave up on. The
  // identifiers are not needed, because the server reads the same information from
  // the exchanged token, but a failure here names the screen worth looking at.
  window.addEventListener("message", function (event) {
    if (!event.origin.endsWith("facebook.com")) {
      return;
    }
    var data = event.data;
    if (typeof data === "string") {
      try {
        data = JSON.parse(data);
      } catch (err) {
        return;
      }
    }
    if (!data || data.type !== "WA_EMBEDDED_SIGNUP" || data.event !== "CANCEL") {
      return;
    }
    if (data.data && data.data.error_message) {
      console.warn("Embedded signup failed:", data.data.error_code, data.data.error_message);
    } else if (data.data && data.data.current_step) {
      console.warn("Embedded signup abandoned at:", data.data.current_step);
    }
  });

  // finishSignup hands the code back to our own endpoint, which exchanges it
  // before it expires and answers with a redirect. Following that redirect and
  // reloading shows the flash the server left for the customer.
  function finishSignup(button, code) {
    button.textContent = "Connecting…";
    warmUp(button)
      .then(function (state) {
        return fetch(button.dataset.callbackUrl, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            "X-CSRF-Token": csrfToken(),
            "X-Requested-With": "XMLHttpRequest",
          },
          credentials: "same-origin",
          body: JSON.stringify({ code: code, state: state }),
        });
      })
      .then(function (response) {
        if (!response.ok) {
          return response.json().then(
            function (body) {
              throw new Error((body && body.message) || "The sign-up could not be completed.");
            },
            function () {
              throw new Error("The sign-up could not be completed. Start again from the Connect WhatsApp button.");
            }
          );
        }
        window.location.reload();
      })
      .catch(function (err) {
        showSignupError(button, err.message || "The sign-up could not be completed.");
      });
  }

  function startSignup(button) {
    if (running) {
      return;
    }
    clearSignupError();
    button.textContent = "Opening Facebook…";
    button.disabled = true;

    loadSDK(button).then(
      function () {
        // Nothing between the click and this call, so the popup it opens is a
        // user gesture the browser cannot refuse.
        window.FB.login(
          function (response) {
            window.clearTimeout(watchdog);
            var code = response && response.authResponse ? response.authResponse.code : "";
            if (!code) {
              running = false;
              showSignupError(button, "Facebook did not return an authorization code. Pick your business account in the dialog and try again.");
              return;
            }
            finishSignup(button, code);
          },
          {
            config_id: button.dataset.configId,
            response_type: "code",
            override_default_response_type: true,
            extras: { setup: {} },
          }
        );
      },
      function (err) {
        showSignupError(button, err.message);
      }
    );

    running = true;
    // Embedded Signup can be left open indefinitely, but a button that never
    // comes back looks broken. Ten minutes is far longer than any real flow.
    watchdog = window.setTimeout(function () {
      if (running) {
        showSignupError(button, "The Facebook dialog was still open after ten minutes. Start again from the Connect WhatsApp button.");
      }
    }, 10 * 60 * 1000);
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

  // Get the state and the SDK in place before anyone clicks, so the click itself
  // has nothing left to wait for.
  function prepare() {
    var button = findConnectButton();
    if (!button) {
      return;
    }
    warmUp(button);
    loadSDK(button).catch(function () {
      // Reported on the click, where there is a button to report it on.
    });
  }

  prepare();
  document.addEventListener("htmx:afterSwap", prepare);
})();
