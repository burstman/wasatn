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
  //
  // The dialog is Meta's own Facebook Login URL, opened by us in a popup.
  //
  // The obvious alternative is the Facebook JavaScript SDK's FB.login(), which is
  // what the SDK exists to do, but it is not usable here. It opens its dialog with
  // window.open from inside a promise callback, so Firefox refuses the popup as
  // un-user-initiated and says nothing at all; its readiness is not observable, so
  // FB.login either runs too early and is rejected with "FB.login() called before
  // FB.init()", or hangs; and the third-party script is one more thing for an ad
  // blocker or a corporate proxy to stop. A plain window.open in the click handler
  // is a user gesture the browser cannot refuse, and it puts no third-party script
  // on the page. Meta still owns the dialog, and config_id still selects the same
  // Embedded Signup configuration.
  //
  // Facebook returns the customer to signup.RedirectURI, which is this page, so the
  // code comes back as query parameters and the server finishes the job.

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

  function warmUp(button) {
    if (statePromise) {
      return statePromise;
    }
    statePromise = requestState(button).catch(function (err) {
      statePromise = null;
      // The preload is an optimisation; a failure is reported on the click.
      throw err;
    });
    statePromise.catch(function () {});
    return statePromise;
  }

  // dialogURL builds the Facebook Login URL for Embedded Signup.
  //
  // This is the URL FB.login would have assembled: config_id selects the Embedded
  // Signup configuration and response_type=code asks for an authorization code
  // instead of a cookie. No scope is requested, because the pages_* scopes were
  // deprecated in Graph v19 and a new app cannot obtain them without App Review.
  function dialogURL(button, state) {
    var query = [
      "client_id=" + encodeURIComponent(button.dataset.appId),
      "redirect_uri=" + encodeURIComponent(button.dataset.redirectUri),
      "response_type=code",
      "config_id=" + encodeURIComponent(button.dataset.configId),
      "state=" + encodeURIComponent(state),
    ].join("&");
    return "https://www.facebook.com/" + button.dataset.version + "/dialog/oauth?" + query;
  }

  // POPUP_FEATURES is a dialog-sized window rather than Meta's default, which is a
  // full tab on some browsers.
  var POPUP_FEATURES = "popup=true,width=520,height=680,menubar=no,toolbar=no,location=yes";

  var signupPopup = null;
  var poll = null;

  // watchPopup reports back once the dialog is over.
  //
  // Facebook navigates the popup to our own page to hand over the code, and that
  // page redirects again once the server has stored the connection, so the popup
  // coming back to our origin means the work is done. Reading its location is
  // blocked until then, which is the expected case rather than an error.
  function watchPopup() {
    if (poll) {
      clearInterval(poll);
    }
    poll = setInterval(function () {
      if (!signupPopup || signupPopup.closed) {
        clearInterval(poll);
        poll = null;
        // The server may have stored the connection before the popup closed, and a
        // flash survives the reload either way, so reload rather than guess.
        window.location.reload();
        return;
      }
      var back = false;
      try {
        back = signupPopup.location.href.indexOf(window.location.origin) === 0;
      } catch (err) {
        return; // Still on facebook.com, which is cross-origin and unreadable.
      }
      if (back) {
        clearInterval(poll);
        poll = null;
        signupPopup.close();
        window.location.reload();
      }
    }, 1500);
  }

  // openDialog opens the Facebook window and sends it to the dialog.
  //
  // The window itself is opened here, synchronously, because a browser only lets
  // a popup open from inside the gesture that asked for it; the dialog URL needs
  // the state value, which may still be in flight. So the popup opens blank and is
  // pointed at Meta as soon as the state arrives. Navigating a window we opened is
  // allowed even once it is on another origin, which is what makes this work.
  function openDialog(button) {
    var popup = window.open("about:blank", "wasatn-whatsapp-signup", POPUP_FEATURES);
    if (!popup) {
      showSignupError(
        button,
        "Your browser blocked the Facebook sign-up window. Allow popups for this site " +
          "(the icon on the left of the address bar) and try again."
      );
      return;
    }
    signupPopup = popup;
    button.textContent = "Finish in the Facebook window…";
    // about:blank inherits our origin, so the placeholder can be styled. If it
    // cannot be written to, the dialog replaces it a moment later anyway.
    try {
      popup.document.write(
        "<!doctype html><title>WasaTN</title>" +
          '<body style="font:15px system-ui;margin:3rem;color:#334155">' +
          "<p>Opening Facebook…</p></body>"
      );
    } catch (err) {
      // Nothing to do: the navigation below is what matters.
    }

    warmUp(button)
      .then(function (state) {
        stateValue = state;
        popup.location = dialogURL(button, state);
        watchPopup();
      })
      .catch(function (err) {
        popup.close();
        signupPopup = null;
        showSignupError(button, err.message || "Could not start the sign-up.");
      });
  }

  function startSignup(button) {
    if (button.disabled) {
      return;
    }
    clearSignupError();

    // A dialog we already opened is somewhere else on the screen, quite possibly
    // behind this tab, so a second click brings it back instead of starting a
    // competing flow.
    if (signupPopup && !signupPopup.closed) {
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

  // Ask for the state up front so the click only has to open a window.
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
