(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const params = new URLSearchParams(location.search);
  const activity = location.pathname === '/activity';
  const form = $('filters');
  const kindLabels = {delivery:'SMTP Deliveries',http:'HTTP requests · all',httpOther:'HTTP · non-poll',httpPoll:'HTTP polls'};
  const fieldKeys = ['recipient','sender','senderDomain','sourceIP','userAgent'];
  const filterKeys = ['kind','search','callerIP',...fieldKeys];
  const REFRESH_MS = 5000;
  let offset = Number(params.get('offset')) || 0, hasMore = false, busy = false;
  let lastSuccess = null, live = true, requestVersion = 0, controller, chartData;
  const expandedEvents = new Set();
  const num = n => Number(n || 0).toLocaleString();
  const bytes = n => n >= 1073741824 ? (n / 1073741824).toFixed(2) + ' GiB' : n >= 1048576 ? (n / 1048576).toFixed(1) + ' MiB' : n >= 1024 ? (n / 1024).toFixed(1) + ' KiB' : num(n) + ' B';
  const date = d => d ? new Date(d).toLocaleString() : 'None';
  const shortDate = d => new Date(d).toLocaleDateString(undefined, {month:'short',day:'numeric'});
  const clock = d => new Date(d).toLocaleTimeString(undefined, {hour:'2-digit',minute:'2-digit',hour12:false});
  const eventClock = d => new Date(d).toLocaleTimeString(undefined, {hour:'2-digit',minute:'2-digit',second:'2-digit',hour12:false});
  const duration = s => s < 3600 ? Math.floor(s / 60) + ' min' : (s / 3600).toFixed(1) + ' h';
  const zone = new Intl.DateTimeFormat(undefined, {timeZoneName:'short'}).formatToParts(new Date()).find(p => p.type === 'timeZoneName')?.value || 'local';
  function node(tag, text, cls) { const e = document.createElement(tag); if (text !== undefined) e.textContent = text; if (cls) e.className = cls; return e; }
  function empty(target, text) { target.replaceChildren(node('p', text, 'admin-empty')); }
  function notice(text) { $('notice').textContent = text; $('notice').hidden = !text; }
  async function get(path, signal) {
    const response = await fetch(path, {cache:'no-store',signal});
    const data = await response.json();
    if (!response.ok) throw new Error(data.error || 'Data unavailable');
    return data;
  }
  document.body.classList.toggle('admin-activity-page',activity);
  $('activity-time-heading').textContent='Time · '+zone;
  $('page-title').textContent = activity ? 'Activity' : 'Overview';
  $('overview').hidden = activity; $('activity').hidden = !activity;
  document.querySelector('.system-details').hidden = activity;
  document.querySelectorAll('.activity-filter').forEach(e => e.hidden = !activity);
  document.querySelector(`[data-page="${activity ? 'activity' : 'overview'}"]`).setAttribute('aria-current', 'page');
  function syncFilters() {
    form.elements.window.value=params.get('window') || '24h';
    form.elements.kind.value=params.get('kind') || '';
    for(const key of fieldKeys) {form.elements[key].value=params.get(key) || '';form.elements[key].dataset.emptyExact=String(params.has(key)&&params.get(key)==='');}
    pendingFilters();
  }
  function pendingFilters() {
    if(!activity)return;
    const dirty=form.elements.window.value!==(params.get('window') || '24h') || form.elements.kind.value!==(params.get('kind') || '') || fieldKeys.some(key=>form.elements[key].value.trim()!==(params.get(key) || ''));
    $('filter-pending').hidden=!dirty;
    $('clear-filters').disabled=!dirty&&!filterKeys.some(key=>params.has(key))&&(params.get('window') || '24h')==='24h';
  }
  function filterChips() {
    const target=$('exact-filter');target.replaceChildren();
    if(!activity) {target.hidden=true;return;}
    const windows={'1h':'Last hour','24h':'Last 24 hours','7d':'Last 7 days','30d':'Last 30 days'};
    const labels={recipient:'Inbox (exact)',sender:'SMTP sender contains',senderDomain:'Sender domain (exact)',sourceIP:'IP address (exact)',userAgent:'HTTP user agent contains',callerIP:'HTTP caller IP (exact)'};
    const filters=[];
    if(params.get('window')&&params.get('window')!=='24h')filters.push(['window',windows[params.get('window')] || params.get('window')]);
    if(params.get('kind'))filters.push(['kind',kindLabels[params.get('kind')] || 'All events']);
    if(params.get('search'))filters.push(['search','Search: '+params.get('search')]);
    for(const key of [...fieldKeys,'callerIP'])if(params.has(key))filters.push([key,labels[key]+': '+(params.get(key) || '(empty)')]);
    for(const [key,label] of filters) {
      const chip=node('button',undefined,'filter-chip');chip.type='button';chip.title=label;
      chip.setAttribute('aria-label','Remove filter: '+label);chip.append(node('span',label),node('span','×','chip-remove'));
      chip.addEventListener('click',()=>{params.delete(key);offset=0;syncFilters();filterChips();resetActivityScroll();refresh();});target.append(chip);
    }
    target.hidden=!filters.length;
  }
  function navigation() {
    const saved = new URLSearchParams(params); if(!saved.has('window'))saved.set('window','24h'); saved.delete('offset');
    document.querySelectorAll('[data-page]').forEach(a => {a.href = new URL(a.href).pathname + '?' + saved;});
  }
  function updateURL() {
    if(!params.has('window'))params.set('window','24h');
    if(activity)params.set('offset',String(offset));
    history.replaceState(null,'',location.pathname + '?' + params);navigation();
  }
  function resetActivityScroll() { if(activity)document.querySelector('.activity-table-wrap').scrollTop=0; }
  function applyFilters() {
    params.set('window',form.elements.window.value);
    for(const key of ['kind',...fieldKeys]) {const value=form.elements[key].value.trim();if(value)params.set(key,value);else if(form.elements[key].dataset.emptyExact!=='true')params.delete(key);}
    params.delete('search');
    offset=0;syncFilters();filterChips();resetActivityScroll();refresh();
  }
  syncFilters();filterChips();navigation();
  function count(label,value,note) {
    const e=node('div',undefined,'admin-count');
    e.append(node('span',label),node('strong',num(value)),node('small',note)); return e;
  }
  function ranking(id, ranks, key, kind) {
    const target=$(id); target.replaceChildren();
    if(!ranks.length) return empty(target,'No activity yet');
    for(const rank of ranks.slice(0,5)) {
      const a=node('a',undefined,'admin-ranking'), line=node('div',undefined,'rank-line');
      a.href='/activity?'+new URLSearchParams({window:form.elements.window.value,kind,[key]:rank.value});
      a.title=rank.value || 'Empty envelope sender';
      line.append(node('strong',rank.value || '(empty sender)'),node('span',num(rank.count)));
      const track=node('div',undefined,'rank-track'), fill=node('i');
      fill.style.width=(rank.count/Math.max(1,ranks[0].count)*100)+'%';track.append(fill);a.append(line,track);target.append(a);
    }
  }
  function details(id, pairs) {
    const target=$(id);target.replaceChildren();
    for(const [label,value] of pairs) {const row=node('div');row.append(node('dt',label),node('dd',String(value)));target.append(row);}
  }
  function health(s, earliest) {
    const smtp=s.smtp || {}, store=s.store || {}, a=s.analytics || {};
    details('runtime',[
      ['Uptime',duration(s.uptimeSeconds)],['Heap memory',bytes(s.memoryBytes)],['Goroutines',num(s.goroutines)],
      ['SMTP connections',`${num(smtp.activeConnections)} / ${num(smtp.connectionLimit)}`],
      ['Mail storage',`${bytes(store.storedBytes)} / ${bytes(s.messageStorageBudgetBytes)}`],['Stored messages',num(store.storedMessages)],
      ['Cleanup errors',num(store.cleanupErrors)],['Last cleanup',store.lastCleanup ? date(store.lastCleanup) : 'Not yet'],
      ['SMTP TLS',smtp.tlsEnabled ? 'Expires '+date(smtp.tlsNotAfter) : 'Disabled']
    ]);
    details('analytics',[
      ['Ingestion',a.available ? 'Available' : 'Unavailable'],['Live pages + WAL',`${bytes(a.storageBytes)} / ${bytes(s.storageBudgetBytes)}`],
      ['Retention limit',`${num(s.retentionHours / 24)} days`],['Earliest retained event',date(earliest || a.earliestEvent)],
      ['Last batch · this process',a.lastIngested ? date(a.lastIngested) : 'None yet'],
      ['Ingestion lag',`${Number(a.lagSeconds || 0).toFixed(1)} s`],['Dropped events',num(a.droppedEvents)],
      ['Write errors',num(a.writeErrors)],['Early evictions',num(a.earlyEvictedEvents)]
    ]);
    const first=earliest || a.earliestEvent;
    const historyText=`${first ? 'History from '+shortDate(first) : 'No retained history'} · ${num(s.retentionHours / 24)}-day limit`;
    const ingestionText=`${a.available ? 'Ingestion '+Number(a.lagSeconds || 0).toFixed(1)+'s behind' : 'Ingestion unavailable'} · ${num(a.droppedEvents)} dropped`;
    $('history').textContent=historyText; $('history').title=first ? 'Earliest retained event: '+date(first)+'. Retention may end early under storage pressure.' : 'No retained events.';
    $('ingestion').textContent=ingestionText; $('ingestion').title='Dropped events since process start; crash losses may be uncounted.';
    $('activity-history').replaceChildren(node('span',historyText),node('span',ingestionText));
    const strip=$('service-status');strip.replaceChildren();
    for(const [label,value,warn] of [
      ['Process','Running',false],['SMTP',`${num(smtp.activeConnections)} / ${num(smtp.connectionLimit)}`,false],
      ['Mail',bytes(store.storedBytes),false],['TLS',smtp.tlsEnabled ? (smtp.tlsNotAfter ? 'Expires '+shortDate(smtp.tlsNotAfter) : 'Enabled') : 'Off',smtp.tlsEnabled && new Date(smtp.tlsNotAfter) < new Date()],
      ['Cleanup',store.cleanupErrors ? num(store.cleanupErrors)+' errors' : 'OK',store.cleanupErrors>0],
      ['Analytics',a.available ? 'Connected' : 'Unavailable',!a.available]
    ]) {const item=node('div',undefined,'service-item'+(warn?' warning':''));if(label==='Process')item.append(node('i',undefined,'service-dot'));item.append(node('span',label),node('strong',value));strip.append(item);}
    const warnings=[];
    if(!a.available)warnings.push('Analytics unavailable. Runtime status is still available.');
    if(a.lagSeconds>30)warnings.push('Ingestion delayed; recent events may be missing.');
    if(a.earlyEvictedEvents>0)warnings.push(`${num(a.earlyEvictedEvents)} events evicted early under storage pressure (this process).`);
    if(a.droppedEvents>0)warnings.push(`${num(a.droppedEvents)} events dropped (this process); totals may be incomplete.`);
    return warnings.join(' ');
  }
  function svgNode(tag, attrs={}, text) {
    const e=document.createElementNS('http://www.w3.org/2000/svg',tag);
    for(const [key,value] of Object.entries(attrs))e.setAttribute(key,value);
    if(text!==undefined)e.textContent=text;return e;
  }
  function drawChart(d) {
    const target=$('chart');
    if(!d || target.clientWidth===0)return;
    const width=Math.max(280,target.clientWidth), height=260, left=44, right=12, top=32, bottom=height-48;
    const plotWidth=width-left-right, plotHeight=bottom-top;
    const peak=Math.max(1,...d.buckets.flatMap(b=>[b.deliveries,b.http]));
    const rough=peak/4, magnitude=10**Math.floor(Math.log10(rough));
    const step=Math.max(1,[1,2,2.5,5,10].map(n=>n*magnitude).find(n=>n>=rough));
    const ceiling=step*4;
    const svg=svgNode('svg',{viewBox:`0 0 ${width} ${height}`,height,role:'group','aria-label':'Traffic chart'});
    const interval=(new Date(d.generatedAt)-new Date(d.since))/d.buckets.length;
    const intervalLabel=interval===86400000 ? '1 day' : interval<3600000 ? `${Number((interval/60000).toFixed(1))} min` : `${Number((interval/3600000).toFixed(1))} h`;
    svg.append(svgNode('text',{x:left,y:14,class:'chart-axis-title'},`Events / ${intervalLabel}`));
    const compact=new Intl.NumberFormat(undefined,{notation:'compact',maximumFractionDigits:1});
    for(let i=0;i<=4;i++) {
      const y=bottom-i*plotHeight/4;
      svg.append(svgNode('line',{x1:left,y1:y,x2:width-right,y2:y,class:'chart-grid'}));
      svg.append(svgNode('text',{x:left-9,y:y+4,'text-anchor':'end',class:'chart-axis'},compact.format(i*step)));
    }
    const ticks=width<500 ? 2 : 4;
    for(let i=0;i<=ticks;i++) {
      const timestamp=new Date(d.since).getTime()+(new Date(d.generatedAt)-new Date(d.since))*i/ticks;
      const label=interval*d.buckets.length>86400000 ? shortDate(timestamp) : clock(timestamp);
      svg.append(svgNode('text',{x:left+plotWidth*i/ticks,y:bottom+21,'text-anchor':i===0?'start':i===ticks?'end':'middle',class:'chart-axis'},label));
    }
    svg.append(svgNode('text',{x:left+plotWidth/2,y:height-3,'text-anchor':'middle',class:'chart-axis-title'},`Time · ${zone}`));
    const slot=plotWidth/d.buckets.length, barWidth=Math.max(1,Math.min(16,(slot-3)/2));
    const focusedBin=target.contains(document.activeElement) ? document.activeElement.dataset.bin : null;
    const tooltip=$('chart-tooltip');tooltip.hidden=true;
    d.buckets.forEach((b,i)=>{
      const x=left+i*slot, end=i===d.buckets.length-1 ? d.generatedAt : d.buckets[i+1].timestamp;
      const description=`${date(b.timestamp)} — ${clock(end)}: ${num(b.deliveries)} SMTP Deliveries, ${num(b.http)} HTTP requests, ${num(b.polling)} polls`;
      const group=svgNode('g',{class:'chart-bin',tabindex:0,'data-bin':i,'aria-label':description});
      group.append(svgNode('title',{},description),svgNode('rect',{x,y:top,width:slot,height:plotHeight,class:'chart-hit'}));
      for(const [value,cls,position] of [[b.deliveries,'chart-delivery',0],[b.http,'chart-http',1]]) {
        const h=value/ceiling*plotHeight;
        group.append(svgNode('rect',{x:x+slot/2-barWidth-1+position*(barWidth+2),y:bottom-h,width:barWidth,height:h,rx:2,class:cls,'pointer-events':'none'}));
      }
      const show=()=>{
        tooltip.replaceChildren(node('strong',`${shortDate(b.timestamp)} · ${clock(b.timestamp)}–${clock(end)}`),node('div',`${num(b.deliveries)} SMTP Deliveries`),node('div',`${num(b.http)} HTTP · ${num(b.polling)} polls`));
        tooltip.hidden=false;
        const panel=target.parentElement;
        tooltip.style.left=Math.max(8,Math.min(panel.clientWidth-tooltip.offsetWidth-8,target.offsetLeft+x+slot/2-tooltip.offsetWidth/2))+'px';
        tooltip.style.top=(target.offsetTop+38)+'px';
      };
      group.addEventListener('pointerenter',show);group.addEventListener('focus',show);
      group.addEventListener('pointerleave',()=>tooltip.hidden=true);group.addEventListener('blur',()=>tooltip.hidden=true);
      group.addEventListener('keydown',e=>{if(e.key==='Escape')tooltip.hidden=true;});
      svg.append(group);
    });
    if(d.deliveries===0&&d.requests===0)svg.append(svgNode('text',{x:left+plotWidth/2,y:top+plotHeight/2,'text-anchor':'middle',class:'chart-axis'},'No traffic in this period'));
    target.replaceChildren(svg);
    if(focusedBin!==null)target.querySelector(`[data-bin="${focusedBin}"]`)?.focus({preventScroll:true});
  }
  function overview(d) {
    $('counts').replaceChildren(count('SMTP Deliveries',d.deliveries,'Per accepted recipient'),count('Recipients',d.recipients,'Distinct inboxes receiving mail'),count('HTTP requests',d.requests,`${num(d.polling)} polling · ${num(d.requests-d.polling)} other`),count('Caller IPs',d.callers,'Distinct HTTP caller IPs'));
    $('counts').setAttribute('aria-busy','false');
    ranking('inboxes',d.inboxes,'recipient','delivery');ranking('senders',d.senderDomains,'senderDomain','delivery');ranking('callers',d.httpCallers,'sourceIP','http');
    chartData=d;drawChart(d);
  }
  function eventEmpty(text) {
    const row=node('tr'), cell=node('td',text,'admin-empty');cell.colSpan=7;row.append(cell);$('events').replaceChildren(row);
  }
  function eventMetadata(e) {
    const content=node('div',undefined,'event-metadata'), list=node('dl');
    const pairs=[['Timestamp',date(e.timestamp)],['Event ID',e.id],['Recipient',e.recipient || '—']];
    if(e.kind==='delivery')pairs.push(['Envelope sender',e.sender || '(empty)'],['Sender domain',e.senderDomain || '(empty)'],['SMTP source IP',e.ip || 'Unknown'],['Size',bytes(e.size)]);
    else pairs.push(['HTTP caller IP',e.ip || 'Unknown'],['Request',e.method+' '+e.route],['Status',e.status],['Duration',Number(e.durationMs).toFixed(1)+' ms'],['Message ID',e.messageId || '—'],['Polling',e.polling?'Identified UI poll':'Not marked as polling'],['Raw user agent',e.userAgent || '(empty)']);
    for(const [label,value] of pairs) {const item=node('div');item.append(node('dt',label),node('dd',String(value)));list.append(item);}
    content.append(list);return content;
  }
  function events(d) {
    const target=$('events');
    const focusedEvent=target.contains(document.activeElement) ? document.activeElement.closest('[data-event-id]')?.dataset.eventId : null;
    const scrollWrap=target.closest('.activity-table-wrap'), scrollTop=scrollWrap.scrollTop;
    target.replaceChildren();
    if(!d.events.length)eventEmpty('No matching events. Change the time range or clear filters.');
    for(const e of d.events) {
      const row=node('tr',undefined,'admin-event');row.dataset.eventId=e.id;
      const timeCell=node('td',undefined,'event-time'), time=node('time',shortDate(e.timestamp)+' '+eventClock(e.timestamp));
      time.dateTime=e.timestamp;time.title=date(e.timestamp);timeCell.append(time);
      const kind=node('td',undefined,'event-kind');kind.append(node('span',e.kind==='delivery'?'Delivery':e.polling?'HTTP poll':'HTTP','admin-kind '+e.kind));
      const recipient=node('td',e.recipient || e.messageId || '—','event-recipient');recipient.title=e.recipient || e.messageId || '';
      const detail=node('td',e.kind==='delivery'?(e.sender || '(empty sender)'):e.method+' '+e.route,'event-detail');detail.title=detail.textContent;
      const source=node('td',undefined,'event-source'),ip=node('span',e.ip || 'Unknown');source.append(node('span',e.kind==='delivery'?'SMTP':'HTTP','source-label'),ip);source.title=(e.kind==='delivery'?'SMTP source IP: ':'HTTP caller IP: ')+(e.ip || 'Unknown');
      const result=node('td',undefined,'event-result');
      if(e.kind==='delivery') {result.textContent=bytes(e.size);result.title='Accepted recipient delivery · '+bytes(e.size);}
      else {result.append(node('span',e.status,'event-status'+(e.status>=400?' error':'')),node('span',' · '+Number(e.durationMs).toFixed(1)+' ms','result-duration'));}
      const action=node('td',undefined,'event-action'),toggle=node('button','›','event-expand');toggle.type='button';toggle.setAttribute('aria-label','Details for '+e.kind+' event at '+date(e.timestamp));
      const expanded=node('tr',undefined,'event-expanded'),cell=node('td');cell.colSpan=7;cell.append(eventMetadata(e));expanded.append(cell);
      // Index-based DOM IDs avoid embedding untrusted event IDs into selector syntax.
      expanded.id='event-detail-'+target.children.length;toggle.setAttribute('aria-controls',expanded.id);
      const setOpen=open=>{expanded.hidden=!open;toggle.setAttribute('aria-expanded',String(open));toggle.title=open?'Hide metadata':'View metadata';};
      setOpen(expandedEvents.has(e.id));
      toggle.addEventListener('click',()=>{const open=toggle.getAttribute('aria-expanded')!=='true';open ? expandedEvents.add(e.id) : expandedEvents.delete(e.id);setOpen(open);});
      action.append(toggle);row.append(timeCell,kind,recipient,detail,source,result,action);target.append(row,expanded);
    }
    for(const id of expandedEvents)if(!d.events.some(e=>e.id===id))expandedEvents.delete(id);
    if(focusedEvent)Array.from(target.children).find(row=>row.dataset.eventId===focusedEvent)?.querySelector('.event-expand')?.focus({preventScroll:true});
    scrollWrap.scrollTop=scrollTop;
    hasMore=d.hasMore;$('previous').disabled=offset===0;$('next').disabled=!hasMore;
    const first=offset+(d.events.length?1:0),last=offset+d.events.length;
    $('page-label').textContent=`Events ${num(first)}–${num(last)}${offset>=d.offsetLimit?' · limit reached':''}`;
    const type=kindLabels[params.get('kind')] || 'All events';
    const windowLabel={'1h':'Last hour','24h':'Last 24 hours','7d':'Last 7 days','30d':'Last 30 days'}[params.get('window')] || 'Last 24 hours';
    $('activity-summary').textContent=type+' · '+windowLabel+' · Latest first';
  }
  function freshness() {
    if($('freshness').dataset.stale==='true')return;
    if(lastSuccess) {
      const age=Math.max(0,Math.floor((Date.now()-new Date(lastSuccess))/1000));
      $('freshness').textContent=age<2 ? 'Updated now' : age<60 ? `Updated ${age}s ago` : `Updated ${Math.floor(age/60)}m ago`;
      $('freshness').title='Last query snapshot: '+date(lastSuccess);
    }
  }
  async function refresh() {
    const version=++requestVersion;
    controller?.abort();
    const activeController=new AbortController();controller=activeController;
    const timeout=setTimeout(()=>activeController.abort(),10000);
    busy=true;$('refresh').setAttribute('aria-busy','true');updateURL();
    if(!lastSuccess) { $('freshness').textContent='Loading…';if(activity)eventEmpty('Loading activity…'); }
    try {
      const [data,status]=await Promise.allSettled([
        get((activity?'/api/activity':'/api/overview')+'?'+params,activeController.signal),get('/api/status',activeController.signal)
      ]);
      if(version!==requestVersion)return;
      let warnings='';
      if(status.status==='fulfilled')warnings=health(status.value,data.status==='fulfilled'?data.value.earliest:null);
      else {warnings='Service status unavailable; ingestion freshness is unknown.';empty($('service-status'),'Status unavailable');}
      if(data.status==='rejected')throw data.reason;
      activity?events(data.value):overview(data.value);
      lastSuccess=data.value.generatedAt;$('freshness').dataset.stale='false';freshness();notice(warnings);
    } catch(err) {
      if(version!==requestVersion)return;
      const message=err.name==='AbortError' ? 'Refresh timed out.' : err instanceof TypeError ? 'Cannot reach the service.' : err.message;
      notice(message+(lastSuccess?' Showing the last snapshot.':''));
      $('freshness').dataset.stale='true';$('freshness').textContent=lastSuccess?'Stale data':'Unavailable';
      if(!lastSuccess&&activity)eventEmpty('Activity unavailable. Try Refresh.');
    } finally {
      clearTimeout(timeout);
      if(version===requestVersion) {busy=false;$('refresh').setAttribute('aria-busy','false');}
    }
  }
  form.addEventListener('submit',e=>{e.preventDefault();applyFilters();});
  if(activity) {
    form.addEventListener('input',e=>{if(fieldKeys.includes(e.target.name))e.target.dataset.emptyExact='false';pendingFilters();});form.addEventListener('change',pendingFilters);
    $('clear-filters').addEventListener('click',()=>{for(const key of filterKeys)params.delete(key);params.set('window','24h');offset=0;syncFilters();filterChips();resetActivityScroll();refresh();});
  } else form.elements.window.addEventListener('change',applyFilters);
  $('refresh').addEventListener('click',refresh);
  $('live-toggle').addEventListener('click',()=>{
    live=!live;$('live-toggle').setAttribute('aria-pressed',String(live));$('live-label').textContent=live?'Live · 5s':'Paused';
    if(live)refresh();
  });
  $('previous').addEventListener('click',()=>{offset=Math.max(0,offset-50);resetActivityScroll();refresh();});
  $('next').addEventListener('click',()=>{if(hasMore){offset+=50;resetActivityScroll();refresh();}});
  document.addEventListener('visibilitychange',()=>{if(!document.hidden&&live)refresh();});
  if(!activity)new ResizeObserver(()=>drawChart(chartData)).observe($('chart'));
  refresh();
  setInterval(()=>{if(!document.hidden&&live&&!busy)refresh();},REFRESH_MS);
  setInterval(()=>{if(!document.hidden)freshness();},1000);
})();
