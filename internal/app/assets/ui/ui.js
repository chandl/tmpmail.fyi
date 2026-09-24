(() => {
  'use strict';

  const LAST_INBOX_KEY = 'tmpmail:last-inbox';
  const THEME_KEY = 'tmpmail:theme';
  const PAGE_SIZE = 25;
  const POLL_MS = 10000;
  const root = document.documentElement;
  const $ = (selector, scope = document) => scope.querySelector(selector);
  const $$ = (selector, scope = document) => Array.from(scope.querySelectorAll(selector));
  const storage = {
    get(key) { try { return localStorage.getItem(key); } catch (_) { return null; } },
    set(key, value) {
      try { value == null ? localStorage.removeItem(key) : localStorage.setItem(key, value); } catch (_) {}
    },
  };
  const statusRegion = $('#status');
  const announce = text => {
    if (!statusRegion) return;
    statusRegion.textContent = '';
    setTimeout(() => { statusRegion.textContent = text; }, 40);
  };

  // Theme: follow the system unless the visitor picked the other one.
  const systemDark = matchMedia('(prefers-color-scheme: dark)');
  const systemTheme = () => (systemDark.matches ? 'dark' : 'light');
  const currentTheme = () => root.dataset.theme || systemTheme();
  const themeButton = $('[data-theme-toggle]');
  const syncThemeButton = () => {
    if (!themeButton) return;
    const label = 'Switch to ' + (currentTheme() === 'dark' ? 'light' : 'dark') + ' theme';
    themeButton.setAttribute('aria-label', label);
    themeButton.title = label;
  };
  themeButton?.addEventListener('click', () => {
    const next = currentTheme() === 'dark' ? 'light' : 'dark';
    if (next === systemTheme()) {
      delete root.dataset.theme;
      storage.set(THEME_KEY, null);
    } else {
      root.dataset.theme = next;
      storage.set(THEME_KEY, next);
    }
    syncThemeButton();
  });
  systemDark.addEventListener?.('change', syncThemeButton);
  syncThemeButton();

  // Dark preview for HTML emails (dark theme only); on unless the visitor turned it off.
  const EMAIL_DARK_KEY = 'tmpmail:email-dark';
  const setEmailDark = on => {
    root.dataset.emailDark = on ? 'on' : 'off';
    $$('[data-email-dark-toggle]').forEach(button => button.setAttribute('aria-pressed', String(on)));
  };
  setEmailDark(storage.get(EMAIL_DARK_KEY) !== 'off');
  document.addEventListener('click', event => {
    if (!event.target.closest?.('[data-email-dark-toggle]')) return;
    const on = root.dataset.emailDark !== 'on';
    storage.set(EMAIL_DARK_KEY, on ? null : 'off');
    setEmailDark(on);
  });

  // Inbox names: two words plus a random suffix.
  const randomInbox = () => {
    const adjectives = ['amber', 'brisk', 'calm', 'daring', 'fuzzy', 'golden', 'lucky', 'mellow', 'nimble', 'solar', 'swift', 'velvet'];
    const nouns = ['badger', 'comet', 'falcon', 'fern', 'otter', 'panda', 'raven', 'river', 'tiger', 'willow', 'wren', 'zebra'];
    const crypto = globalThis.crypto;
    const pick = words => words[Math.floor(Math.random() * words.length)];
    if (crypto?.getRandomValues) {
      const bytes = new Uint32Array(2);
      crypto.getRandomValues(bytes);
      return pick(adjectives) + '-' + pick(nouns) + '-' + Array.from(bytes, value => value.toString(36)).join('');
    }
    return pick(adjectives) + '-' + pick(nouns) + '-' + Math.random().toString(36).slice(2, 12);
  };
  const isInboxName = value => value && !/[\/@\\]/.test(value);
  const inboxURL = name => '/?inbox=' + encodeURIComponent(name);

  const body = document.body;
  const address = body.dataset.address || '';
  const offset = Math.max(0, Number(body.dataset.offset) || 0);
  const form = $('#inbox-form');
  const input = $('#inbox');

  if (form && input) {
    const domain = input.dataset.domain || '';
    let leaving = false;
    const normalize = () => {
      let value = input.value.trim();
      // Accept a pasted full address for this domain.
      if (domain && value.toLowerCase().endsWith('@' + domain.toLowerCase())) value = value.slice(0, -domain.length - 1);
      input.value = value;
      return value;
    };
    form.addEventListener('submit', event => {
      const value = normalize();
      if (leaving || value === input.defaultValue) { event.preventDefault(); return; }
      if (!value) { event.preventDefault(); input.value = input.defaultValue; return; }
      if (isInboxName(value)) storage.set(LAST_INBOX_KEY, value);
      leaving = true;
    });
    const fit = () => { input.style.width = 'calc(' + Math.max(input.value.length || input.placeholder.length, 4) + 'ch + var(--field-pad))'; };
    input.addEventListener('input', fit);
    input.addEventListener('change', () => {
      if (normalize() && input.value !== input.defaultValue) form.requestSubmit();
    });
    input.addEventListener('keydown', event => {
      if (event.key === 'Escape') { input.value = input.defaultValue; fit(); input.blur(); }
    });
    if (!input.value) {
      const name = storage.get(LAST_INBOX_KEY);
      const next = isInboxName(name) ? name : randomInbox();
      storage.set(LAST_INBOX_KEY, next);
      location.replace(inboxURL(next));
      return;
    }
    if (address && isInboxName(input.value)) storage.set(LAST_INBOX_KEY, input.value);
  }

  $$('[data-new-address]').forEach(button => button.addEventListener('click', () => {
    const next = randomInbox();
    storage.set(LAST_INBOX_KEY, next);
    location.assign(inboxURL(next));
  }));

  const copyText = async text => {
    if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
    const area = document.createElement('textarea');
    area.value = text;
    area.setAttribute('readonly', '');
    area.style.cssText = 'position:fixed;opacity:0';
    document.body.append(area);
    area.select();
    const ok = document.execCommand('copy');
    area.remove();
    if (!ok) throw new Error('copy failed');
  };
  const copyTimers = new WeakMap();
  document.addEventListener('click', async event => {
    const button = event.target.closest?.('[data-copy]');
    if (!button || !button.dataset.copy) return;
    try {
      await copyText(button.dataset.copy);
      button.classList.add('is-copied');
      announce(button.dataset.copied || 'Address copied');
      clearTimeout(copyTimers.get(button));
      copyTimers.set(button, setTimeout(() => button.classList.remove('is-copied'), 1600));
    } catch (_) {
      announce('Could not copy. Select the text and copy it manually.');
    }
  });

  // Developer snippets: fill in this server's origin, the inbox, and the message ID.
  const urlPart = value => encodeURIComponent(value).replace(/%40/g, '@').replace(/'/g, '%27');
  const placeCursor = snippet => $$('.snippet', snippet.closest('.dev-tools')).forEach(other => other.classList.toggle('has-cursor', other === snippet));
  const renderSnippets = (scope = document) => {
    $$('[data-cmd]', scope).forEach(code => {
      const id = code.closest('[data-message-id]')?.dataset.messageId || '';
      code.textContent = code.dataset.cmd
        .replaceAll('{origin}', location.origin)
        .replaceAll('{inbox}', urlPart(address))
        .replaceAll('{id}', urlPart(id));
      const button = code.closest('.snippet')?.querySelector('[data-copy]');
      if (button) button.dataset.copy = code.textContent;
    });
    // The blinking cursor rests on the last command until another one is hovered.
    $$('.dev-tools', scope).forEach(box => {
      const snippets = $$('.snippet', box);
      if (snippets.length) placeCursor(snippets[snippets.length - 1]);
    });
  };
  renderSnippets();
  const followCursor = event => {
    const snippet = event.target.closest?.('.dev-tools .snippet');
    if (snippet) placeCursor(snippet);
  };
  document.addEventListener('pointerover', followCursor);
  document.addEventListener('focusin', followCursor);

  const refresh = () => location.reload();
  $$('[data-refresh]').forEach(link => link.addEventListener('click', event => {
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    refresh();
  }));

  // Friendly times, in the visitor's own timezone.
  const fullTime = date => date.toLocaleString([], { weekday: 'short', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
  const relativeTime = date => {
    const minutes = Math.floor((Date.now() - date) / 60000);
    if (minutes < 1) return 'Just now';
    if (minutes < 60) return minutes + ' min ago';
    if (new Date().toDateString() === date.toDateString()) return date.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
    return date.toLocaleDateString([], { month: 'short', day: 'numeric' });
  };
  const expiresIn = date => {
    const minutes = Math.ceil((date - Date.now()) / 60000);
    if (minutes <= 0) return 'Expired';
    if (minutes < 120) return 'Expires in ' + minutes + ' min';
    return 'Expires in ' + Math.round(minutes / 60) + ' hr';
  };
  const renderTimes = (scope = document) => {
    $$('time.local-time', scope).forEach(element => {
      const date = new Date(element.dateTime);
      if (Number.isNaN(date.valueOf())) return;
      element.title = date.toLocaleString([], { dateStyle: 'full', timeStyle: 'long' });
      element.textContent = element.dataset.format === 'relative' ? relativeTime(date) : fullTime(date);
    });
    $$('[data-expires]', scope).forEach(element => {
      const date = new Date(element.dataset.expires);
      if (Number.isNaN(date.valueOf())) return;
      element.textContent = expiresIn(date);
      element.title = 'Deleted at ' + fullTime(date);
    });
  };
  renderTimes();
  setInterval(renderTimes, 30000);

  const mailbox = $('.mailbox');
  const known = new Set();
  let addMessages = null;

  if (mailbox) {
    const list = $('.message-list', mailbox);
    const reader = $('.reader-pane', mailbox);
    const backButton = $('[data-back]', mailbox);
    const mobile = matchMedia('(max-width: 720px)');
    const rows = () => $$('.message-row', list);
    const articleFor = row => row && document.getElementById('m-' + row.dataset.id);
    let current = null;

    const ensureFrame = article => {
      const holder = $('[data-frame]', article);
      if (!article.classList.contains('has-html') || !holder || $('iframe', holder)) return;
      const frame = document.createElement('iframe');
      frame.className = 'html-frame';
      frame.title = 'Formatted email: ' + ($('.message-subject', article)?.textContent || '(no subject)');
      frame.setAttribute('sandbox', 'allow-popups allow-popups-to-escape-sandbox');
      frame.src = '/ui/messages/' + encodeURIComponent(article.dataset.messageId) + '/html';
      holder.append(frame);
    };
    const showView = (article, view) => {
      article.classList.toggle('show-plain', view === 'plain');
      $$('.segmented button', article).forEach(button => button.setAttribute('aria-pressed', String(button.dataset.view === view)));
      if (view === 'html') ensureFrame(article);
    };
    const addAttachments = (article, attachments) => {
      const section = document.createElement('section');
      section.className = 'attachments';
      section.setAttribute('aria-label', 'Attachments');
      section.innerHTML = '<h3>Attachments</h3><ul></ul>';
      const icon = $('#message-template')?.content.querySelector('.attachments svg');
      attachments.forEach(attachment => {
        const link = document.createElement('a');
        link.href = '/api/v1/messages/' + encodeURIComponent(article.dataset.messageId) + '/attachments/' + attachment.index;
        link.setAttribute('download', '');
        const name = document.createElement('span');
        name.textContent = attachment.filename;
        if (icon) link.append(icon.cloneNode(true));
        link.append(name);
        const item = document.createElement('li');
        item.append(link);
        $('ul', section).append(item);
      });
      $('.headers', article).before(section);
    };
    // Mirrors blockedImages.Label in ui.go.
    const setBlockedImages = (article, total, pixels) => {
      const note = $('[data-blocked-images]', article);
      if (!note) return;
      const plural = (n, word) => n + ' ' + word + (n === 1 ? '' : 's');
      note.hidden = total === 0;
      $('[data-blocked-label]', note).textContent =
        total === 0 ? '' :
        pixels === total ? (total === 1 ? 'Tracking pixel blocked' : plural(total, 'tracking pixel') + ' blocked') :
        pixels === 0 ? plural(total, 'image') + ' blocked for privacy' :
        plural(total, 'image') + ' blocked, incl. ' + plural(pixels, 'tracking pixel');
    };
    const loadMessage = article => {
      if (!article || article.dataset.loaded === 'true' || article.dataset.loading === 'true') return;
      article.dataset.loading = 'true';
      article.classList.add('is-loading');
      const text = $('.plain-body pre', article);
      const headers = $('.headers pre', article);
      fetch('/ui/messages/' + encodeURIComponent(article.dataset.messageId))
        .then(response => (response.ok ? response.json() : Promise.reject(response.status)))
        .then(message => {
          text.textContent = message.text;
          headers.textContent = message.headers;
          if (message.attachments?.length && !$('.attachments', article)) addAttachments(article, message.attachments);
          article.classList.toggle('has-html', message.hasHtml);
          setBlockedImages(article, message.blockedImages || 0, message.trackingPixels || 0);
          article.dataset.loaded = 'true';
          if (article.classList.contains('is-active') && !article.classList.contains('show-plain')) ensureFrame(article);
        })
        .catch(() => {
          article.classList.remove('has-html');
          text.textContent = 'This message is no longer available. It may have expired.';
        })
        .finally(() => {
          delete article.dataset.loading;
          article.classList.remove('is-loading');
        });
    };

    const openReader = () => {
      if (!mobile.matches || mailbox.classList.contains('is-reading')) return;
      mailbox.classList.add('is-reading');
      history.pushState({ reader: true }, '', location.href);
      mailbox.scrollIntoView({ block: 'start' });
      $('.message.is-active .message-subject', reader)?.focus({ preventScroll: true });
    };
    const closeReader = () => {
      if (!mailbox.classList.contains('is-reading')) return;
      mailbox.classList.remove('is-reading');
      current?.focus({ preventScroll: true });
      current?.scrollIntoView({ block: 'nearest' });
    };
    const select = (row, { open = false, focus = false, remember = true } = {}) => {
      if (!row) return;
      const article = articleFor(row);
      if (row !== current) {
        rows().forEach(other => {
          const selected = other === row;
          other.setAttribute('aria-selected', String(selected));
          other.tabIndex = selected ? 0 : -1;
        });
        $$('.message.is-active', reader).forEach(other => other.classList.remove('is-active'));
        article?.classList.add('is-active');
        reader.scrollTop = 0;
        current = row;
        if (remember) history.replaceState(history.state, '', '#m-' + row.dataset.id);
      }
      if (article) {
        loadMessage(article);
        if (!article.classList.contains('show-plain')) ensureFrame(article);
      }
      if (focus) {
        row.focus({ preventScroll: true });
        row.scrollIntoView({ block: 'nearest' });
      }
      if (open) openReader();
    };
    const move = step => {
      const all = rows();
      const index = all.indexOf(current);
      const next = all[Math.min(all.length - 1, Math.max(0, index + step))];
      if (next && next !== current) select(next, { focus: true, open: mailbox.classList.contains('is-reading') });
    };

    list.addEventListener('click', event => {
      const row = event.target.closest('.message-row');
      if (!row || event.metaKey || event.ctrlKey || event.shiftKey) return;
      event.preventDefault();
      select(row, { open: true });
    });
    list.addEventListener('keydown', event => {
      if (event.key === 'Home' || event.key === 'End') {
        event.preventDefault();
        const all = rows();
        select(event.key === 'Home' ? all[0] : all[all.length - 1], { focus: true });
      }
    });
    reader.addEventListener('click', event => {
      const button = event.target.closest('.segmented button');
      if (button) showView(button.closest('.message'), button.dataset.view);
    });
    backButton?.addEventListener('click', () => (history.state?.reader ? history.back() : closeReader()));
    window.addEventListener('popstate', () => { if (!history.state?.reader) closeReader(); });
    mobile.addEventListener?.('change', () => { if (!mobile.matches) mailbox.classList.remove('is-reading'); });

    document.addEventListener('keydown', event => {
      if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey) return;
      const target = event.target;
      if (target.closest?.('input, textarea, select, [contenteditable]')) return;
      const inList = target === document.body || list.contains(target);
      const key = event.key;
      if (key === 'j' || (key === 'ArrowDown' && inList)) { event.preventDefault(); move(1); }
      else if (key === 'k' || (key === 'ArrowUp' && inList)) { event.preventDefault(); move(-1); }
      else if (key === 'r' || key === 'R') { event.preventDefault(); refresh(); }
      else if (key === 'Escape' && mailbox.classList.contains('is-reading')) backButton?.click();
    });

    rows().forEach(row => known.add(row.dataset.id));
    const fromHash = location.hash.startsWith('#m-') && $('#row-' + CSS.escape(location.hash.slice(3)), list);
    select(fromHash || rows()[0], { remember: false });

    const countLabel = $('[data-count]');
    const updateCount = () => {
      if (!countLabel || offset !== 0) return;
      const count = rows().length;
      countLabel.textContent = $('[data-older]') ? 'Messages 1–' + count : count + (count === 1 ? ' message' : ' messages');
    };
    const rowTemplate = $('#row-template');
    const messageTemplate = $('#message-template');
    // "Name <addr>" -> { name, address }; mirrors senderFromHeaders on the server.
    const parseSender = value => {
      const match = /^\s*"?([^"<]*?)"?\s*<([^>]+)>\s*$/.exec(value || '');
      return match ? { name: match[1].trim(), address: match[2].trim() } : { name: '', address: (value || '').trim() };
    };
    addMessages = messages => {
      if (!rowTemplate || !messageTemplate) return false;
      // Oldest first, so the newest ends up on top.
      messages.slice().reverse().forEach(message => {
        const stamp = (element, attribute, value) => element && element.setAttribute(attribute, value);
        const row = rowTemplate.content.firstElementChild.cloneNode(true);
        row.id = 'row-' + message.id;
        row.dataset.id = message.id;
        row.href = '#m-' + message.id;
        const sender = parseSender(message.from);
        $('.row-from', row).textContent = sender.name || sender.address || 'Unknown sender';
        stamp($('.row-from', row), 'title', sender.address);
        $('.row-subject', row).textContent = message.subject || '(no subject)';
        stamp($('time', row), 'datetime', message.received);
        row.querySelector('[data-expires]').dataset.expires = message.expiresAt;

        const article = messageTemplate.content.firstElementChild.cloneNode(true);
        article.id = 'm-' + message.id;
        article.dataset.messageId = message.id;
        article.setAttribute('aria-labelledby', 'subject-' + message.id);
        const subject = $('.message-subject', article);
        subject.id = 'subject-' + message.id;
        subject.textContent = message.subject || '(no subject)';
        const from = $('.meta-from', article);
        from.textContent = sender.name || sender.address || 'Unknown sender';
        if (sender.name && sender.address) {
          const addr = document.createElement('span');
          addr.className = 'addr';
          addr.textContent = '<' + sender.address + '>';
          from.append(' ', addr);
        }
        $('.meta-to', article).textContent = message.recipient || address;
        stamp($('time', article), 'datetime', message.received);
        article.querySelector('[data-expires]').dataset.expires = message.expiresAt;

        renderTimes(row);
        renderTimes(article);
        renderSnippets(article);
        $('[data-email-dark-toggle]', article)?.setAttribute('aria-pressed', String(root.dataset.emailDark === 'on'));
        list.prepend(row);
        reader.append(article);
        known.add(message.id);
        row.classList.add('is-new');
        row.addEventListener('animationend', () => row.classList.remove('is-new'), { once: true });
      });
      updateCount();
      return true;
    };
  }

  // Gentle auto-refresh: one small request every 10s while the tab is visible,
  // only on the newest page, backing off if the server asks us to slow down.
  if (address && offset === 0 && !$('.notice')) {
    let delay = POLL_MS;
    let timer = 0;
    let inFlight = false;
    const indicators = $$('[data-live]');
    const describe = checked => {
      $$('[data-live-every]').forEach(element => { element.textContent = 'Checks for new mail every ' + Math.round(delay / 1000) + ' seconds.'; });
      if (!checked) return;
      const time = checked.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit', second: '2-digit' });
      $$('[data-live-last]').forEach(element => { element.textContent = 'Last checked ' + time + ' · click to check now.'; });
    };
    indicators.forEach(element => { element.hidden = false; });
    $$('[data-refresh-mode]').forEach(element => { element.textContent = 'automatically'; });
    describe();
    // Restart the ring so it fills over exactly the time until the next check.
    const setRing = state => indicators.forEach(element => {
      element.classList.remove('is-counting', 'is-checking');
      if (!state) return;
      element.style.setProperty('--poll', delay + 'ms');
      void element.offsetWidth;
      element.classList.add(state);
    });
    const schedule = () => {
      clearTimeout(timer);
      if (document.visibilityState !== 'visible') { setRing(null); return; }
      timer = setTimeout(poll, delay);
      setRing('is-counting');
    };
    const poll = async (manual = false) => {
      if (inFlight) return;
      inFlight = true;
      setRing('is-checking');
      try {
        const response = await fetch('/api/v1/inboxes/' + encodeURIComponent(address) + '?limit=' + PAGE_SIZE + '&offset=0', { cache: 'no-store' });
        if (response.status === 429 || response.status === 503) { delay = Math.min(delay * 2, 120000); return; }
        if (!response.ok) return;
        delay = POLL_MS;
        describe(new Date());
        const page = await response.json();
        const fresh = (page.messages || []).filter(message => !known.has(message.id));
        if (!fresh.length) { if (manual) announce('No new messages'); return; }
        if (!addMessages || !addMessages(fresh)) { location.reload(); return; }
        announce(fresh.length === 1 ? 'New message: ' + (fresh[0].subject || '(no subject)') : fresh.length + ' new messages');
      } catch (_) {
        // Network hiccup: try again on the next tick.
      } finally {
        inFlight = false;
        schedule();
      }
    };
    $$('.live-button').forEach(button => button.addEventListener('click', () => { clearTimeout(timer); poll(true); }));
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible') poll();
      else { clearTimeout(timer); setRing(null); }
    });
    schedule();
  }
})();
