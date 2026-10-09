(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const params = new URLSearchParams(location.search);
  const activity = location.pathname === '/activity';
  const form = $('filters');
  let offset = Number(params.get('offset')) || 0, hasMore = false, busy = false;
  let lastSuccess = null;
  const num = n => Number(n || 0).toLocaleString();
  const bytes = n => n >= 1073741824 ? (n / 1073741824).toFixed(2) + ' GiB' : n >= 1048576 ? (n / 1048576).toFixed(1) + ' MiB' : n >= 1024 ? (n / 1024).toFixed(1) + ' KiB' : num(n) + ' B';
  const date = d => d ? new Date(d).toLocaleString() : 'No retained events';
  const duration = s => s < 3600 ? Math.floor(s / 60) + ' min' : (s / 3600).toFixed(1) + ' h';
  function node(tag, text, cls) { const e = document.createElement(tag); if (text !== undefined) e.textContent = text; if (cls) e.className = cls; return e; }
  function empty(target, text) { target.replaceChildren(node('p', text, 'admin-empty')); }
  function notice(text) { $('notice').textContent = text; $('notice').hidden = !text; }
  async function get(path) { const response = await fetch(path, {cache:'no-store'}); const data = await response.json(); if (!response.ok) throw new Error(data.error || 'Data unavailable'); return data; }
  $('page-title').textContent = activity ? 'Activity' : 'Overview';
  $('overview').hidden = activity; $('activity').hidden = !activity;
  document.querySelectorAll('.activity-filter').forEach(e => e.hidden = !activity);
  document.querySelector(`[data-page="${activity ? 'activity' : 'overview'}"]`).setAttribute('aria-current', 'page');
  for (const key of ['window', 'kind', 'search']) if (params.has(key)) form.elements[key].value = params.get(key);
  for (const key of ['recipient','senderDomain','callerIP']) if (activity && params.has(key)) {
    $('exact-filter').hidden = false;
    $('exact-filter').append(node('span', `${key}: ${params.get(key) || '(empty)'} `));
  }
  if (!$('exact-filter').hidden) { const clear = node('a','Clear exact filters'); const remaining = new URLSearchParams(params); for(const key of ['recipient','senderDomain','callerIP','offset']) remaining.delete(key); clear.href = '/activity?' + remaining; $('exact-filter').append(clear); }
  function navigation() { const saved = new URLSearchParams(params); saved.set('window',form.elements.window.value); saved.delete('offset'); document.querySelectorAll('[data-page]').forEach(a => {a.href = new URL(a.href).pathname + '?' + saved;}); }
  navigation();
  function updateURL() {
    params.set('window', form.elements.window.value);
    if (activity) { for (const key of ['kind','search']) { const value = form.elements[key].value; value ? params.set(key,value) : params.delete(key); } params.set('offset',String(offset)); }
    history.replaceState(null,'',location.pathname + '?' + params);
    navigation();
  }
  function count(label,value,note) { const e=node('div',undefined,'admin-count');e.append(node('span',label),node('strong',num(value)));if(note)e.append(node('small',note));return e; }
  function ranking(id, ranks, key, kind) {
    const target=$(id); target.replaceChildren();
    if(!ranks.length) return empty(target,'No activity in this window.');
    for(const rank of ranks) {const a=node('a',undefined,'admin-ranking'); const p=new URLSearchParams({window:form.elements.window.value,kind,[key]:rank.value});a.href='/activity?'+p;a.append(node('strong',rank.value || '(empty envelope sender)'),node('span',num(rank.count)));target.append(a);}
  }
  function details(id, pairs) {const target=$(id);target.replaceChildren();for(const [label,value] of pairs){const row=node('div');row.append(node('dt',label),node('dd',String(value)));target.append(row);}}
  function health(s, earliest) {
    const smtp=s.smtp || {}, store=s.store || {}, a=s.analytics || {};
    details('runtime',[
      ['Process uptime',duration(s.uptimeSeconds)],['Heap memory',bytes(s.memoryBytes)],['Goroutines',num(s.goroutines)],
      ['SMTP connections',`${num(smtp.activeConnections)} / ${num(smtp.connectionLimit)}`],
      ['Message storage',`${bytes(store.storedBytes)} / ${bytes(s.messageStorageBudgetBytes)}`],['Stored messages',num(store.storedMessages)],
      ['Cleanup errors · this process',num(store.cleanupErrors)],['Last cleanup',store.lastCleanup ? date(store.lastCleanup) : 'Not yet'],
      ['SMTP TLS',smtp.tlsEnabled ? 'Enabled · expires '+date(smtp.tlsNotAfter) : 'Disabled']
    ]);
    details('analytics',[
      ['Ingestion',a.available ? 'Available' : 'Unavailable'],['Budget usage · live pages + WAL',`${bytes(a.storageBytes)} / ${bytes(s.storageBudgetBytes)} budget`],
      ['Configured retention',`${num(s.retentionHours)} hours`],['Last batch ingested · this process',a.lastIngested ? date(a.lastIngested) : 'None this process'],
      ['Ingestion lag',`${Number(a.lagSeconds || 0).toFixed(1)} seconds`],['Dropped events · this process',num(a.droppedEvents)],
      ['Write errors · this process',num(a.writeErrors)],['Early evictions · this process',num(a.earlyEvictedEvents)]
    ]);
    $('ingestion').textContent=`Ingestion: ${a.available ? 'available' : 'unavailable'} · last batch this process ${a.lastIngested ? date(a.lastIngested) : 'none'} · lag ${Number(a.lagSeconds || 0).toFixed(1)} s · dropped ${num(a.droppedEvents)} · early evictions ${num(a.earlyEvictedEvents)} (counters since process start).`;
    $('history').textContent=`Configured retention: ${num(s.retentionHours)} hours. Earliest retained event: `+date(earliest || a.earliestEvent)+'. Retention is a maximum; the storage budget can evict events earlier.';
    const warnings=[];
    if(!a.available)warnings.push('Analytics unavailable. Runtime status is still available.');
    if(a.lagSeconds>30)warnings.push('Analytics ingestion is behind; recent events may be missing.');
    if(a.earlyEvictedEvents>0)warnings.push('Storage pressure has evicted history before its configured expiry.');
    if(a.droppedEvents>0)warnings.push('Some events were dropped; totals are best-effort.');
    return warnings.join(' ');
  }
  function overview(d) {
    $('counts').replaceChildren(count('Recipient deliveries',d.deliveries),count('Distinct recipients',d.recipients),count('HTTP requests',d.requests,`${num(d.polling)} identifiable polling · ${num(d.requests-d.polling)} other`),count('Distinct HTTP caller IPs',d.callers));
    ranking('inboxes',d.inboxes,'recipient','delivery');ranking('senders',d.senderDomains,'senderDomain','delivery');ranking('callers',d.httpCallers,'callerIP','http');
    const chart=$('chart');chart.replaceChildren();const max=Math.max(1,...d.buckets.flatMap(b=>[b.deliveries,b.http]));
    for(const b of d.buckets){const group=node('div',undefined,'admin-chart-group');group.title=`${date(b.timestamp)}: ${num(b.deliveries)} deliveries; ${num(b.http)} HTTP (${num(b.polling)} polling)`;for(const [value,cls] of [[b.deliveries,''],[b.http,'http']]){const bar=node('div',undefined,'admin-bar '+cls);bar.style.height=(value/max*100)+'%';group.append(bar);}chart.append(group);}
    chart.setAttribute('role','img');chart.setAttribute('aria-label',`Traffic: ${num(d.deliveries)} recipient deliveries and ${num(d.requests)} HTTP requests across 24 equal intervals. Green represents deliveries; gray represents HTTP.`);
    $('chart-range').textContent=`${date(d.since)} — ${date(d.generatedAt)} · green: deliveries · gray: HTTP · ${d.buckets.length} equal intervals`;
  }
  function events(d) {
    const target=$('events');target.replaceChildren();
    if(!d.events.length)empty(target,'No matching retained activity in this window. Try a wider range or clear filters.');
    for(const e of d.events){const row=node('article',undefined,'admin-event'), heading=node('div',undefined,'admin-event-heading'),time=node('time',date(e.timestamp));time.dateTime=e.timestamp;heading.append(node('span',e.kind==='delivery'?'Delivery':'HTTP'+(e.polling?' · UI polling':''),'admin-kind '+e.kind),time);row.append(heading);
      if(e.kind==='delivery'){row.append(node('p',`${e.recipient} ← ${e.sender || '(empty envelope sender)'}`),node('p',`SMTP source IP: ${e.ip || '(unknown)'} · ${bytes(e.size)}`,'muted'));}
      else {row.append(node('p',`${e.method} ${e.route} · ${e.status} · ${Number(e.durationMs).toFixed(1)} ms`),node('p',`HTTP caller IP: ${e.ip || '(unknown)'}${e.recipient ? ' · Inbox: '+e.recipient : ''}${e.messageId ? ' · Message: '+e.messageId : ''}`,'muted'),node('p','Raw user agent: '+(e.userAgent || '(empty)'),'muted'));}
      target.append(row);
    }
    hasMore=d.hasMore;$('previous').disabled=offset===0;$('next').disabled=!hasMore;
    $('page-label').textContent=`${num(offset+ (d.events.length?1:0))}–${num(offset+d.events.length)}${offset>=d.offsetLimit?' · pagination limit reached':''}`;
  }
  async function refresh() {
    if(busy)return;busy=true;$('refresh').disabled=true;updateURL();
    $('freshness').textContent=lastSuccess ? 'Refreshing… last snapshot '+date(lastSuccess) : 'Loading…';
    if(!lastSuccess&&activity)empty($('events'),'Loading activity…');
    try {
      const dataPromise=get((activity?'/api/activity':'/api/overview')+'?'+params);
      const statusPromise=get('/api/status');
      const [data,status]=await Promise.allSettled([dataPromise,statusPromise]);
      let warnings='';
      if(status.status==='fulfilled')warnings=health(status.value,data.status==='fulfilled'?data.value.earliest:null);
      else warnings='Runtime and ingestion status unavailable. The activity query snapshot does not establish ingestion freshness.';
      if(data.status==='rejected')throw data.reason;
      activity?events(data.value):overview(data.value);
      lastSuccess=data.value.generatedAt;$('freshness').textContent='Snapshot '+date(lastSuccess)+' · refreshes every 30 seconds';notice(warnings);
    } catch(err) {notice(err.message+(lastSuccess?' Showing the previous snapshot; data is stale.':''));$('freshness').textContent=lastSuccess?'Stale snapshot '+date(lastSuccess):'Data unavailable';if(!lastSuccess&&activity)empty($('events'),'Activity unavailable. Retry with Refresh.');}
    finally {busy=false;$('refresh').disabled=false;}
  }
  form.addEventListener('submit',e=>{e.preventDefault();offset=0;refresh();});
  form.elements.window.addEventListener('change',()=>{offset=0;refresh();});
  form.elements.kind.addEventListener('change',()=>{offset=0;refresh();});
  $('refresh').addEventListener('click',refresh);
  $('previous').addEventListener('click',()=>{offset=Math.max(0,offset-50);refresh();});
  $('next').addEventListener('click',()=>{if(hasMore){offset+=50;refresh();}});
  refresh();setInterval(()=>{if(!document.hidden)refresh();},30000);
})();
