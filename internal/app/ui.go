package app

const uiCSS = `
.random{background:#263554!important;color:#dce5f8!important}.random:hover{background:#334365!important}.html-toggle{margin-top:14px;background:#263554!important;color:#dce5f8!important;padding:6px 10px!important;font-size:12px!important}.html-frame{width:100%;min-height:360px;margin-top:12px;border:1px solid #263554;border-radius:8px;background:#fff}.mailbox{display:grid;grid-template-columns:270px minmax(0,1fr);min-height:470px;margin:18px -22px -22px;border-top:1px solid #263554}.message-list{overflow:auto;padding:9px;border-right:1px solid #263554;background:#0e1628}.message-item{display:block;width:100%;border:0;border-radius:9px;background:transparent;color:#c7d2e8;padding:11px;text-align:left;cursor:pointer}.message-item:hover{background:#17233b}.message-item.active{background:#263b62;color:#fff}.message-item-subject{display:block;overflow:hidden;font-weight:700;text-overflow:ellipsis;white-space:nowrap}.message-item-meta{display:block;margin-top:3px;color:#8fa0c1;font-size:12px}.message-item-preview{display:-webkit-box;overflow:hidden;margin-top:5px;color:#9aa7bf;font-size:12px;line-height:1.35;-webkit-box-orient:vertical;-webkit-line-clamp:2}.message-reader{min-width:0;background:#121a2d}.mailbox .message{display:none;border:0;margin:0;padding:22px}.mailbox .message.active{display:block}.mailbox .subject{font-size:20px;letter-spacing:-.02em}.mailbox .details{margin-top:8px;padding-bottom:16px;border-bottom:1px solid #263554}.mailbox .section-label{margin-top:20px}.mailbox .message pre{min-height:150px;background:#0e1628}.mailbox details pre{min-height:auto}.pagination{display:flex;align-items:center;justify-content:flex-end;gap:8px;margin:14px 0 0}.pagination a{border:1px solid #334365;border-radius:7px;color:#cbd6ec;padding:5px 9px;text-decoration:none;font-size:12px}.pagination a:hover{background:#202d48}@media(max-width:700px){.mailbox{display:block;margin:18px -16px -16px}.message-list{max-height:220px;border-right:0;border-bottom:1px solid #263554}.mailbox .message{padding:16px}.mailbox .subject{font-size:18px}}
`

const uiScript = `
(() => {
  const panel = document.querySelector('.panel');
  const messages = [...document.querySelectorAll('.panel > .message')];
  const form = document.getElementById('inbox-form');
  const input = document.getElementById('inbox');
  const makeRandomInbox = () => {
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
  if (form && input) {
    const random = document.createElement('button');
    random.type = 'button';
    random.className = 'random';
    random.textContent = 'New random';
    random.addEventListener('click', () => {
      input.value = makeRandomInbox();
      form.requestSubmit();
    });
    const refresh = document.createElement('button');
    refresh.type = 'button';
    refresh.className = 'random';
    refresh.textContent = 'Refresh';
    refresh.addEventListener('click', () => window.location.reload());
    form.querySelector('.lookup')?.append(random, refresh);
  }
  if (!panel || messages.length === 0) return;

  const mailbox = document.createElement('div');
  mailbox.className = 'mailbox';
  const list = document.createElement('aside');
  list.className = 'message-list';
  list.setAttribute('aria-label', 'Messages');
  const reader = document.createElement('section');
  reader.className = 'message-reader';
  mailbox.append(list, reader);

  const select = (index) => {
    buttons.forEach((button, i) => {
      const active = i === index;
      button.classList.toggle('active', active);
      button.setAttribute('aria-selected', String(active));
      messages[i].classList.toggle('active', active);
    });
  };
  const buttons = messages.map((message, index) => {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'message-item';
    const subject = message.querySelector('.subject')?.textContent || '(no subject)';
    const details = message.querySelector('.details')?.textContent || '';
    const body = message.querySelector('pre')?.textContent.replace(/\s+/g, ' ').trim() || '';
    button.innerHTML = '<span class="message-item-subject"></span><span class="message-item-meta"></span><span class="message-item-preview"></span>';
    button.querySelector('.message-item-subject').textContent = subject;
    button.querySelector('.message-item-meta').textContent = details;
    button.querySelector('.message-item-preview').textContent = body;
    button.addEventListener('click', () => select(index));
    list.append(button);
    message.classList.add('email-view');
    reader.append(message);
    return button;
  });
  panel.append(mailbox);
  select(0);

  const address = panel.querySelector('.meta > span')?.textContent;
  const params = new URLSearchParams(window.location.search);
  const offset = Math.max(0, Number(params.get('offset') || 0));
  if (address && Number.isFinite(offset)) {
    fetch('/api/inboxes/' + encodeURIComponent(address) + '?limit=25&offset=' + offset)
      .then(response => response.ok ? response.json() : null)
      .then(page => {
        if (!page) return;
        page.messages.forEach((item, index) => {
          const message = messages[index];
          if (!message) return;
          const toggle = document.createElement('button');
          toggle.type = 'button';
          toggle.className = 'html-toggle';
          toggle.textContent = 'View HTML';
          toggle.addEventListener('click', async () => {
            const url = '/ui/messages/' + encodeURIComponent(item.id) + '/html';
            const response = await fetch(url);
            if (!response.ok) { toggle.textContent = 'No HTML version'; return; }
            const frame = document.createElement('iframe');
            frame.className = 'html-frame';
            frame.sandbox = '';
            frame.src = url;
            toggle.replaceWith(frame);
          });
          message.querySelector('.details')?.after(toggle);
        });
        if (!page.hasMore && offset === 0) return;
        const navigation = document.createElement('nav');
        navigation.className = 'pagination';
        navigation.setAttribute('aria-label', 'Inbox pages');
        const link = (label, nextOffset) => {
          const next = new URLSearchParams(window.location.search);
          next.set('offset', String(nextOffset));
          const anchor = document.createElement('a');
          anchor.href = '?' + next.toString();
          anchor.textContent = label;
          navigation.append(anchor);
        };
        if (offset > 0) link('Previous', Math.max(0, offset - 25));
        if (page.hasMore) link('Next', offset + 25);
        panel.insertBefore(navigation, mailbox);
      })
      .catch(() => {});
  }
})();
`
