(() => {
  "use strict";

  let navigationInProgress = false;

  const formError = (message = "") => {
    const el = document.getElementById("error");
    if (!el) return;
    el.textContent = message;
    el.hidden = !message;
  };

  const readJSON = async (response) => {
    try { return await response.json(); } catch { return {}; }
  };

  const bindAuthForm = (form, payload) => {
    if (!form) return;

    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      if (navigationInProgress) return;
      formError("");

      const submit = form.querySelector("button[type=submit]");
      if (submit) {
        submit.disabled = true;
        submit.dataset.originalText = submit.textContent;
        submit.textContent = "Please wait…";
      }

      try {
        const response = await fetch(payload.url, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Accept: "application/json"
          },
          credentials: "same-origin",
          cache: "no-store",
          body: JSON.stringify(payload.body())
        });

        const data = await readJSON(response);
        if (!response.ok) {
          throw new Error(data.error || payload.fallback);
        }

        navigationInProgress = true;
        window.location.replace("/");
      } catch (err) {
        if (navigationInProgress) return;
        formError(err?.message || payload.fallback);
        if (submit) {
          submit.disabled = false;
          submit.textContent = submit.dataset.originalText || "Submit";
        }
      }
    });
  };

  const loginForm = document.getElementById("login-form");
  bindAuthForm(loginForm, {
    url: "/api/auth/login",
    fallback: "Could not sign in.",
    body: () => ({
      login: loginForm.login.value.trim(),
      password: loginForm.password.value
    })
  });

  const registerForm = document.getElementById("register-form");
  bindAuthForm(registerForm, {
    url: "/api/auth/register",
    fallback: "Could not create account.",
    body: () => ({
      username: registerForm.username.value.trim(),
      email: registerForm.email.value.trim(),
      password: registerForm.password.value
    })
  });
})();
