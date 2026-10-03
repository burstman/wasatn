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

  // loadSdk resolves once window.FB is initialised. A second call reuses the
  // first promise so repeated clicks do not inject the script twice.
  function loadSdk(version) {
    if (sdkPromise) {
      return sdkPromise;
    }
    sdkPromise = new Promise(function (resolve, reject) {
      if (window.FB) {
        window.FB.init({ xfbml: false, version: version });
        resolve(window.FB);
        return;
      }
      window.fbAsyncInit = function () {
        window.FB.init({ xfbml: false, version: version });
        resolve(window.FB);
      };
      var script = document.createElement("script");
      script.src = SDK_SRC;
      script.async = true;
      script.crossOrigin = "anonymous";
      script.onerror = function () {
        sdkPromise = null;
        reject(new Error("Could not load Facebook's login script."));
      };
      document.head.appendChild(script);
    });
    return sdkPromise;
  }

  // requestState asks our own server for the state value that proves this signup
  // round trip. It is fetched per attempt rather than cached, because the server
  // stores it in the session and clears it on use.
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

  function startSignup(button) {
    button.disabled = true;
    button.textContent = "Opening Facebook…";
    clearSignupError();

    requestState(button)
      .then(function (state) {
        return loadSdk(button.dataset.version).then(function (fb) {
          fb.login(function (authResponse) {
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
            // config_id selects the Embedded Signup configuration; response_type
            // code is what returns an authorization code instead of a cookie.
            config_id: button.dataset.configId,
            response_type: "code",
            scope: "pages_show_list,pages_read_engagement",
            state: state,
          });
        });
      })
      .catch(function (err) {
        showSignupError(button, err.message || "Could not start the sign-up.");
      });
  }

  document.addEventListener("click", function (event) {
    var button = event.target.closest && event.target.closest("#connect-whatsapp");
    if (!button) {
      return;
    }
    event.preventDefault();
    if (!button.disabled) {
      startSignup(button);
    }
  });
})();