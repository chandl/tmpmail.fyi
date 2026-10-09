const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const vm = require('node:vm');
const script = readFileSync(join(__dirname, 'assets/ui/ui.js'), 'utf8');

// A minimal DOM surface drives the actual shipped script, including selection,
// fetch, and polling. No browser packages or copied implementation are needed.
function harness({ ids = ['a', 'b'], hasMore = false, hash = '', loaded = true, detailStatus = 200 } = {}) {
  const timers = [];
  const requests = [];
  const replacements = [];
  let reloads = 0;
  let nextPage = { messages: ids.map(id => ({ id })), hasMore };
  const classes = (...values) => {
    const set = new Set(values);
    return {
      contains: name => set.has(name), add: name => set.add(name), remove: name => set.delete(name),
      toggle(name, enabled) { if (enabled ?? !set.has(name)) set.add(name); else set.delete(name); },
    };
  };
  const element = (dataset = {}) => ({
    dataset, classList: classes(), attributes: {}, listeners: {}, scrollTop: 0,
    querySelector: () => null, querySelectorAll: () => [],
    setAttribute(name, value) { this.attributes[name] = value; },
    addEventListener(name, listener) { this.listeners[name] = listener; },
    focus() {}, scrollIntoView() {},
  });
  const rows = ids.map(id => Object.assign(element({ id }), { tabIndex: -1 }));
  const articles = ids.map((id, index) => {
    const article = element({ messageId: id, loaded: index === 0 && loaded ? 'true' : 'false' });
    article.text = { textContent: index === 0 && loaded ? 'already rendered' : '' };
    article.headers = { textContent: '' };
    article.classList = classes(...(index === 0 ? ['is-active'] : []));
    article.querySelector = selector => ({ '.plain-body pre': article.text, '.headers pre': article.headers })[selector] || null;
    return article;
  });
  const list = element();
  list.querySelectorAll = selector => selector === '.message-row' ? rows : [];
  list.querySelector = selector => rows.find(row => selector === '#row-' + row.dataset.id) || null;
  const reader = element();
  reader.querySelectorAll = selector => selector === '.message.is-active' ? articles.filter(article => article.classList.contains('is-active')) : [];
  const mailbox = element();
  mailbox.querySelector = selector => ({ '.message-list': list, '.reader-pane': reader })[selector] || null;
  const status = { textContent: '' };
  const body = element({ address: 'build@mail.test', offset: '0', hasMore: String(hasMore) });
  const document = Object.assign(element(), {
    documentElement: element(), body, visibilityState: 'visible',
    querySelector: selector => ({ '.mailbox': mailbox, '#status': status })[selector] || null,
    querySelectorAll: selector => selector === '.message-list .message-row' ? rows : [],
    getElementById: id => articles.find(article => 'm-' + article.dataset.messageId === id),
  });
  const location = { hash, href: 'https://mail.test/?inbox=build' + hash, origin: 'https://mail.test', reload() { reloads++; } };
  const context = {
    document, location, window: element(), navigator: {}, CSS: { escape: value => value },
    localStorage: { getItem: () => null },
    matchMedia: () => ({ matches: false, addEventListener() {} }),
    history: { state: null, replaceState(_state, _title, url) { replacements.push(url); }, pushState() {} },
    setTimeout(fn, ms) { timers.push({ fn, ms }); return timers.length; }, clearTimeout() {}, setInterval() {},
    fetch(url) {
      requests.push(url);
      if (url.startsWith('/api/')) return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(nextPage) });
      if (detailStatus === 'network') return Promise.reject(new Error('offline'));
      return Promise.resolve({ ok: detailStatus === 200, status: detailStatus, json: () => Promise.resolve({ text: 'loaded body', headers: 'From: sender', hasHtml: false }) });
    },
  };
  vm.runInNewContext(script, context);
  return {
    rows, articles, requests, replacements,
    get reloads() { return reloads; },
    async settle() { await new Promise(resolve => setImmediate(resolve)); },
    async poll(page) {
      nextPage = page;
      const timer = timers.findLast(timer => timer.ms === 10000);
      assert.ok(timer, 'poll must be scheduled');
      await timer.fn();
    },
    click(index) {
      list.listeners.click({ target: { closest: () => rows[index] }, preventDefault() {} });
    },
  };
}

test('server-rendered first body is reused; other bodies load only on selection', async () => {
  const ui = harness();
  assert.deepEqual(ui.requests, []);
  ui.click(1);
  await ui.settle();
  assert.deepEqual(ui.requests, ['/ui/messages/b']);
  assert.equal(ui.articles[1].text.textContent, 'loaded body');
  ui.click(1);
  assert.equal(ui.requests.length, 1);
});

test('hash selects a non-first message and missing hash falls back and is corrected', async () => {
  const selected = harness({ hash: '#m-b' });
  await selected.settle();
  assert.deepEqual(selected.requests, ['/ui/messages/b']);
  assert.deepEqual(selected.replacements, []);
  const expired = harness({ hash: '#m-gone' });
  assert.deepEqual(expired.replacements, ['#m-a']);
  assert.deepEqual(expired.requests, []);
});

test('unchanged authoritative page does not refresh or grow DOM', async () => {
  const ui = harness();
  for (let i = 0; i < 3; i++) await ui.poll({ messages: [{ id: 'a' }, { id: 'b' }], hasMore: false });
  assert.equal(ui.reloads, 0);
  assert.equal(ui.rows.length, 2);
});

for (const [name, page] of Object.entries({
  arrival: { messages: [{ id: 'new' }, { id: 'a' }, { id: 'b' }], hasMore: false },
  expiry: { messages: [{ id: 'a' }], hasMore: false },
  empty: { messages: [], hasMore: false },
  reorder: { messages: [{ id: 'b' }, { id: 'a' }], hasMore: false },
  pagination: { messages: [{ id: 'a' }, { id: 'b' }], hasMore: true },
  overflow: { messages: Array.from({ length: 25 }, (_, i) => ({ id: 'new-' + i })), hasMore: true },
})) {
  test(name + ' reloads the bounded page with authoritative counts and offsets', async () => {
    const ui = harness();
    await ui.poll(page);
    assert.equal(ui.reloads, 1);
    assert.equal(ui.rows.length, 2, 'never append to an old snapshot');
  });
}

for (const status of [404, 500, 'network']) {
  test('message load handles ' + status + ' and allows retry', async () => {
    const ui = harness({ loaded: false, detailStatus: status });
    await ui.settle();
    assert.match(ui.articles[0].text.textContent, status === 404 ? /may have expired/ : /Select it again to retry/);
    ui.click(0);
    await ui.settle();
    assert.equal(ui.requests.length, 2);
    assert.equal(ui.articles[0].dataset.loaded, 'false');
  });
}
