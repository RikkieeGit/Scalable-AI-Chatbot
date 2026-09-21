(() => {
  "use strict";

  if (window.__sys32AIInitialized) return;
  window.__sys32AIInitialized = true;

  const escapeHTML = (value) => String(value ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
  }[c]));

  const renderMarkdown = (source) => {
    let text = String(source ?? "").replace(/\r\n?/g, "\n");
    const blocks = [];
    text = text.replace(/```([A-Za-z0-9_+.-]*)\n([\s\S]*?)```/g, (_, lang, code) => {
      const i = blocks.length;
      blocks.push({ lang: lang || "code", code: code.replace(/\n$/, "") });
      return `@@CODEBLOCK_${i}@@`;
    });
    let safe = escapeHTML(text)
      .replace(/^### (.+)$/gm, "<h3>$1</h3>")
      .replace(/^## (.+)$/gm, "<h2>$1</h2>")
      .replace(/^# (.+)$/gm, "<h1>$1</h1>")
      .replace(/\*\*(.+?)\*\*/g, "<strong>$1</strong>")
      .replace(/__(.+?)__/g, "<strong>$1</strong>")
      .replace(/`([^`\n]+)`/g, "<code>$1</code>")
      .replace(/^\s*[-*] (.+)$/gm, "<li>$1</li>")
      .replace(/(<li>.*<\/li>\n?)+/g, (m) => `<ul>${m}</ul>`)
      .replace(/^> (.+)$/gm, "<blockquote>$1</blockquote>")
      .replace(/\n{2,}/g, "</p><p>")
      .replace(/\n/g, "<br>");
    safe = `<p>${safe}</p>`;
    blocks.forEach((block, i) => {
      safe = safe.replace(`@@CODEBLOCK_${i}@@`, `<div class="code-wrap"><div class="code-header"><span>${escapeHTML(block.lang)}</span><button type="button" class="copy-code">Copy</button></div><pre><code>${escapeHTML(block.code)}</code></pre></div>`);
    });
    return safe.replace(/^<p><\/?p>$/, "");
  };

  function initSys32AI() {
    const form = document.getElementById("composer");
    const textarea = document.getElementById("composer-input");
    const messages = document.getElementById("messages");
    const emptyState = document.getElementById("empty-state");
    const conversationList = document.getElementById("conversation-list");
    const search = document.getElementById("conversation-search");
    const sendButton = document.getElementById("send-button");
    const modeToggle = document.getElementById("mode-toggle");
    const modeText = document.getElementById("mode-text");
    const composerMode = document.getElementById("composer-mode-label");
    const quotaDisplay = document.getElementById("quota-display");
    const quotaBar = document.getElementById("quota-bar");
    const clearView = document.getElementById("clear-view-btn");
    const newChat = document.getElementById("new-chat-btn");
    const logout = document.getElementById("logout-btn");
    const sidebar = document.getElementById("sidebar");

    if (!form || !textarea || !messages || !sendButton) return;

    let csrfToken = "";
    let currentConversationId = null;
    let currentMode = "fast";
    let controller = null;
    let generating = false;
    let conversations = [];
    let sendInProgress = false;

    const redirectToLogin = () => {
      window.location.replace("/login");
    };

    const authFetch = async (url, options = {}) => {
      const opts = { ...options, credentials: "same-origin" };
      opts.headers = new Headers(opts.headers || {});
      const method = (opts.method || "GET").toUpperCase();
      if (csrfToken && method !== "GET" && method !== "HEAD") {
        opts.headers.set("X-CSRF-Token", csrfToken);
      }
      const response = await fetch(url, opts);
      if (response.status === 401) redirectToLogin();
      return response;
    };

    const setAccount = (user) => {
      const username = String(user?.username || "User");
      const email = String(user?.email || "Signed in");
      const usernameEl = document.getElementById("account-username");
      const emailEl = document.getElementById("account-email");
      const avatarEl = document.getElementById("account-avatar");
      if (usernameEl) usernameEl.textContent = username;
      if (emailEl) emailEl.textContent = email;
      if (avatarEl) avatarEl.textContent = username.charAt(0).toUpperCase() || "U";
    };

    const scrollBottom = () => { messages.scrollTop = messages.scrollHeight; };
    const closeSidebar = () => { sidebar?.classList.remove("open"); document.body.classList.remove("sidebar-open"); };
    const openSidebar = () => { sidebar?.classList.add("open"); document.body.classList.add("sidebar-open"); };
    const resize = () => { textarea.style.height = "auto"; textarea.style.height = `${Math.min(textarea.scrollHeight, 210)}px`; };
    const updateCount = () => {
      const count = document.getElementById("char-count");
      if (count) count.textContent = `${textarea.value.length} / ${textarea.maxLength}`;
    };
    const setGenerating = (value) => {
      generating = value;
      sendButton.classList.toggle("stop-mode", value);
      textarea.disabled = value;
    };

    const clearMessages = () => {
      messages.innerHTML = "";
      if (emptyState) { emptyState.classList.remove("hidden"); messages.appendChild(emptyState); }
    };

    const appendUserMessage = (text) => {
      emptyState?.classList.add("hidden");
      const article = document.createElement("article");
      article.className = "message user-message";
      article.innerHTML = `<div class="user-bubble"></div>`;
      article.querySelector(".user-bubble").textContent = text;
      messages.appendChild(article);
      scrollBottom();
    };

    const createAssistantMessage = () => {
      emptyState?.classList.add("hidden");
      const article = document.createElement("article");
      article.className = "message assistant-message";
      article.innerHTML = `<div class="message-avatar">✦</div><div class="message-body"><div class="message-meta"><strong>Sys32.AI</strong><span class="stream-meta">Qwen 3.6 · Groq</span></div><div class="message-content stream-content"></div><div class="message-actions"><button type="button" class="action-link copy-message">Copy</button></div></div>`;
      messages.appendChild(article);
      return { root: article, content: article.querySelector(".stream-content"), raw: "" };
    };

    const setAssistantContent = (assistant, raw, render = false) => {
      assistant.raw = raw;
      if (render) assistant.content.innerHTML = renderMarkdown(raw);
      else assistant.content.textContent = raw;
    };

    const renderConversationList = (items) => {
      if (!conversationList) return;
      const query = (search?.value || "").trim().toLowerCase();
      const filtered = query ? items.filter((c) => String(c.title || "").toLowerCase().includes(query)) : items;
      conversationList.innerHTML = "";
      if (!filtered.length) {
        conversationList.innerHTML = `<div class="conversation-empty">${query ? "No matching chats" : "No conversations yet"}</div>`;
        return;
      }
      const today = new Date().toDateString();
      const groups = new Map();
      for (const item of filtered) {
        const label = new Date(item.updated_at).toDateString() === today ? "Today" : "Recent";
        if (!groups.has(label)) groups.set(label, []);
        groups.get(label).push(item);
      }
      for (const [label, group] of groups) {
        const heading = document.createElement("div");
        heading.className = "group-label";
        heading.textContent = label;
        conversationList.appendChild(heading);
        for (const item of group) {
          const wrapper = document.createElement("div");
          wrapper.className = `conversation-item${Number(item.id) === Number(currentConversationId) ? " active" : ""}`;
          const button = document.createElement("button");
          button.className = "conversation-btn";
          button.type = "button";
          button.textContent = item.title || "New chat";
          button.title = item.title || "New chat";
          button.addEventListener("click", () => loadConversation(item.id));
          const del = document.createElement("button");
          del.className = "conversation-delete";
          del.type = "button";
          del.textContent = "✕";
          del.setAttribute("aria-label", `Delete ${item.title || "conversation"}`);
          del.addEventListener("click", (e) => { e.stopPropagation(); deleteConversation(item.id); });
          wrapper.append(button, del);
          conversationList.appendChild(wrapper);
        }
      }
    };

    const loadConversations = async () => {
      const response = await authFetch("/api/conversations", { cache: "no-store" });
      if (response.status === 401) return;
      if (!response.ok) return;
      conversations = await response.json();
      renderConversationList(conversations);
    };

    const loadUsage = async () => {
      const response = await authFetch("/api/usage", { cache: "no-store" });
      if (!response.ok) return;
      const data = await response.json();
      const used = Number(data.used_tokens || 0), limit = Number(data.daily_limit || 1);
      const remaining = Number(data.remaining_tokens ?? Math.max(0, limit - used));
      if (quotaDisplay) quotaDisplay.textContent = `${remaining.toLocaleString()} left`;
      if (quotaBar) quotaBar.style.width = `${Math.min(100, (used / Math.max(limit, 1)) * 100)}%`;
      const model = document.getElementById("system-model-name");
      if (model) model.textContent = String(data.model || "Qwen 3.6").replace(/^qwen\/qwen/i, "Qwen");
    };

    const loadConversation = async (id) => {
      if (generating) return;
      const response = await authFetch(`/api/conversations/${encodeURIComponent(id)}`, { cache: "no-store" });
      if (response.status === 401) return;
      if (!response.ok) return;
      const data = await response.json();
      currentConversationId = data.conversation.id;
      messages.innerHTML = "";
      if (!data.messages?.length) {
        emptyState?.classList.remove("hidden");
        if (emptyState) messages.appendChild(emptyState);
      } else {
        emptyState?.classList.add("hidden");
        for (const msg of data.messages) {
          if (msg.role === "user") {
            appendUserMessage(msg.content);
            continue;
          }
          if (msg.role === "assistant") {
            const assistant = createAssistantMessage();
            const status = msg.status || "completed";
            let content = msg.content || "";
            if (status === "pending" && !content) content = "Generating…";
            if (status === "cancelled" && !content) content = "Generation cancelled.";
            if (status === "failed" && !content) content = "Generation failed.";
            setAssistantContent(assistant, content, status === "completed");
            const meta = assistant.root.querySelector(".stream-meta");
            if (meta) {
              const label = status === "completed" ? "" : ` · ${status}`;
              meta.textContent = `${msg.model || "Qwen 3.6"} · ${msg.provider || "AI"}${label}`;
            }
            if (status === "failed" || status === "cancelled") {
              assistant.content.classList.add("error-text");
            }
          }
        }
      }
      renderConversationList(conversations);
      closeSidebar();
      scrollBottom();
    };

    const createConversation = async () => {
      const response = await authFetch("/api/conversations", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title: "New chat" })
      });
      if (!response.ok) throw new Error(response.status === 401 ? "Session expired. Please sign in again." : "Could not create conversation.");
      return response.json();
    };

    const deleteConversation = async (id) => {
      if (!window.confirm("Delete this conversation?")) return;
      const response = await authFetch(`/api/conversations/${encodeURIComponent(id)}`, { method: "DELETE" });
      if (!response.ok) return;
      if (Number(currentConversationId) === Number(id)) {
        currentConversationId = null;
        clearMessages();
      }
      await loadConversations();
    };

    const stopGeneration = async () => {
      if (!generating || !currentConversationId) return;
      sendButton.disabled = true;
      try {
        await authFetch("/api/chat/stop", {
          method: "POST",
          headers: { "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8" },
          body: new URLSearchParams({ conversation_id: String(currentConversationId) }),
          cache: "no-store"
        });
      } catch {}
      controller?.abort();
      sendButton.disabled = false;
    };

    const send = async () => {
      if (generating || sendInProgress) return;
      const message = textarea.value.trim();
      if (!message) return textarea.focus();
      sendInProgress = true;
      try {
        if (!currentConversationId) {
          const conversation = await createConversation();
          currentConversationId = conversation.id;
        }
        appendUserMessage(message);
        textarea.value = "";
        resize();
        updateCount();
        const assistant = createAssistantMessage();
        controller = new AbortController();
        setGenerating(true);
        const response = await authFetch("/api/chat/stream", {
          method: "POST",
          headers: { "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8", Accept: "text/event-stream" },
          body: new URLSearchParams({ message, mode: currentMode, conversation_id: String(currentConversationId) }),
          cache: "no-store", signal: controller.signal
        });
        if (!response.ok) {
          let msg = `Request failed (${response.status})`;
          try { const body = await response.json(); msg = body.error || msg; } catch {}
          if (response.status === 409 && !msg) msg = "A response is already being generated for this conversation.";
          throw new Error(msg);
        }
        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          const frames = buffer.split("\n\n");
          buffer = frames.pop() || "";
          for (const frame of frames) {
            const line = frame.split("\n").find((x) => x.startsWith("data:"));
            if (!line) continue;
            const payload = JSON.parse(line.slice(5).trim());
            if (payload.type === "token") {
              assistant.raw += payload.content || "";
              setAssistantContent(assistant, assistant.raw, false);
              scrollBottom();
            } else if (payload.type === "done") {
              if (payload.conversation_id) currentConversationId = payload.conversation_id;
              const statusValue = payload.status || "completed";
              if (statusValue === "completed") {
                setAssistantContent(assistant, assistant.raw, true);
                assistant.content.classList.remove("error-text");
              } else {
                const fallback = statusValue === "cancelled" ? "Generation cancelled." : "Generation failed.";
                setAssistantContent(assistant, assistant.raw || fallback, false);
                assistant.content.classList.add("error-text");
              }
              const meta = assistant.root.querySelector(".stream-meta");
              if (meta) {
                const status = statusValue !== "completed" ? ` · ${statusValue}` : "";
                meta.textContent = `${payload.model || "Qwen 3.6"} · ${payload.provider || "AI"}${status}`;
              }
            } else if (payload.type === "error") {
              if (!assistant.raw) assistant.raw = payload.message || "Generation failed.";
              setAssistantContent(assistant, assistant.raw, false);
              assistant.content.classList.add("error-text");
              const meta = assistant.root.querySelector(".stream-meta");
              if (meta) meta.textContent = "Qwen 3.6 · generation failed";
            }
          }
        }
      } catch (error) {
        if (error.name !== "AbortError") {
          const existing = messages.querySelector(".assistant-message:last-child .message-content");
          if (existing && !existing.textContent) {
            existing.textContent = error.message || "Generation failed.";
            existing.classList.add("error-text");
          }
        }
      } finally {
        controller = null;
        setGenerating(false);
        sendInProgress = false;
        await Promise.allSettled([loadConversations(), loadUsage()]);
      }
    };

    const startNewChat = () => {
      if (generating) return;
      currentConversationId = null;
      clearMessages();
      closeSidebar();
      textarea.focus();
      renderConversationList(conversations);
    };

    modeToggle?.addEventListener("click", () => {
      currentMode = currentMode === "fast" ? "think" : "fast";
      if (modeText) modeText.textContent = currentMode === "fast" ? "Fast" : "Think";
      if (composerMode) composerMode.textContent = currentMode === "fast" ? "fast-engine" : "think-engine";
    });
    newChat?.addEventListener("click", startNewChat);
    clearView?.addEventListener("click", startNewChat);
    search?.addEventListener("input", () => renderConversationList(conversations));
    textarea.addEventListener("input", () => { resize(); updateCount(); });
    textarea.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        send();
      }
    });
    form.addEventListener("submit", (e) => { e.preventDefault(); send(); });
    sendButton.addEventListener("click", () => { if (generating) stopGeneration(); });

    logout?.addEventListener("click", async () => {
      if (generating) controller?.abort();
      logout.disabled = true;
      try {
        const response = await authFetch("/api/auth/logout", { method: "POST" });
        if (!response.ok) {
          let message = `Sign out failed (${response.status})`;
          try { const body = await response.json(); message = body.error || message; } catch {}
          throw new Error(message);
        }
        window.location.replace("/login");
      } catch (error) {
        logout.disabled = false;
        if (error?.message === "Failed to fetch") redirectToLogin();
        else window.alert(error?.message || "Could not sign out. Please try again.");
      }
    });

    const passwordModal = document.getElementById("password-modal");
    const passwordForm = document.getElementById("password-form");
    const passwordError = document.getElementById("password-error");
    const changePasswordButton = document.getElementById("change-password-btn");
    const passwordModalClose = document.getElementById("password-modal-close");
    const passwordCancel = document.getElementById("password-cancel");
    const passwordSubmit = document.getElementById("password-submit");

    const showPasswordError = (message = "") => {
      if (!passwordError) return;
      passwordError.textContent = message;
      passwordError.classList.toggle("hidden", !message);
    };
    const closePasswordModal = () => {
      passwordModal?.classList.add("hidden");
      passwordForm?.reset();
      showPasswordError("");
    };
    const openPasswordModal = () => {
      passwordModal?.classList.remove("hidden");
      document.getElementById("current-password")?.focus();
    };
    changePasswordButton?.addEventListener("click", openPasswordModal);
    passwordModalClose?.addEventListener("click", closePasswordModal);
    passwordCancel?.addEventListener("click", closePasswordModal);
    passwordModal?.addEventListener("click", (event) => {
      if (event.target === passwordModal) closePasswordModal();
    });
    document.addEventListener("keydown", (event) => {
      if (event.key === "Escape" && !passwordModal?.classList.contains("hidden")) closePasswordModal();
    });

    passwordForm?.addEventListener("submit", async (event) => {
      event.preventDefault();
      showPasswordError("");
      const currentPassword = document.getElementById("current-password")?.value || "";
      const newPassword = document.getElementById("new-password")?.value || "";
      const confirmPassword = document.getElementById("confirm-password")?.value || "";
      if (newPassword !== confirmPassword) {
        showPasswordError("New passwords do not match.");
        return;
      }
      passwordSubmit.disabled = true;
      passwordSubmit.textContent = "Changing…";
      try {
        const response = await authFetch("/api/auth/change-password", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ current_password: currentPassword, new_password: newPassword, confirm_password: confirmPassword })
        });
        if (!response.ok) {
          let message = `Password change failed (${response.status})`;
          try { const body = await response.json(); message = body.error || message; } catch {}
          throw new Error(message);
        }
        closePasswordModal();
        window.alert("Password changed successfully. Please sign in again.");
        window.location.replace("/login");
      } catch (error) {
        if (error?.message) showPasswordError(error.message);
      } finally {
        passwordSubmit.disabled = false;
        passwordSubmit.textContent = "Change password";
      }
    });

    document.querySelectorAll("[data-sidebar-open]").forEach((el) => el.addEventListener("click", openSidebar));
    document.querySelectorAll("[data-sidebar-close]").forEach((el) => el.addEventListener("click", closeSidebar));
    document.querySelectorAll("[data-prompt]").forEach((el) => el.addEventListener("click", () => { textarea.value = el.dataset.prompt || ""; resize(); updateCount(); textarea.focus(); form.requestSubmit(); }));
    document.addEventListener("click", (e) => {
      const codeButton = e.target.closest(".copy-code");
      if (codeButton) {
        const code = codeButton.closest(".code-wrap")?.querySelector("code")?.textContent || "";
        navigator.clipboard?.writeText(code);
        codeButton.textContent = "Copied";
        setTimeout(() => { codeButton.textContent = "Copy"; }, 1000);
        return;
      }
      const msgButton = e.target.closest(".copy-message");
      if (msgButton) {
        const text = msgButton.closest(".assistant-message")?.querySelector(".message-content")?.innerText || "";
        navigator.clipboard?.writeText(text);
        msgButton.textContent = "Copied";
        setTimeout(() => { msgButton.textContent = "Copy"; }, 1000);
      }
    });

    const bootstrap = async () => {
      const response = await fetch("/api/auth/me", { credentials: "same-origin", cache: "no-store" });
      if (response.status === 401) { redirectToLogin(); return; }
      if (!response.ok) throw new Error("Could not establish the session.");
      const data = await response.json();
      csrfToken = data.csrf_token || "";
      setAccount(data.user);
      await Promise.all([loadConversations(), loadUsage()]);
      resize();
      updateCount();
    };
    bootstrap().catch(() => redirectToLogin());
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", initSys32AI, { once: true });
  else initSys32AI();
})();
