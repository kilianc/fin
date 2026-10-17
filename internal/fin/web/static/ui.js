// fin --ui draws each page from fin's local JSON endpoints. No framework and
// no build step: markup comes from the html`` tag, which escapes every value
// it is given unless the value is itself markup from html``.
(() => {
  'use strict';

  const cfg = JSON.parse(document.getElementById('fin-config').textContent);
  const main = document.getElementById('main');

  // --- markup ---

  class Raw { constructor(s) { this.s = s; } }
  const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
  const esc = s => String(s).replace(/[&<>"']/g, c => ESC[c]);
  const part = v => v == null || v === false ? '' : v instanceof Raw ? v.s : Array.isArray(v) ? v.map(part).join('') : esc(v);
  const html = (strings, ...vals) => new Raw(strings.reduce((out, s, i) => out + s + (i < vals.length ? part(vals[i]) : ''), ''));
  const put = (el, markup) => { el.innerHTML = part(markup); };
  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  // --- numbers and dates ---

  const MINUS = '\u2212';
  const fmt = (v, digits) => Math.abs(v).toLocaleString('en-US', { minimumFractionDigits: digits, maximumFractionDigits: digits });
  const money = (v, digits = 0) => (Math.round(v * 10 ** digits) < 0 ? MINUS : '') + '$' + fmt(v, digits);
  const money2 = v => money(v, 2);
  const signed = (v, digits = 0) => (Math.round(v * 10 ** digits) < 0 ? MINUS : '+') + '$' + fmt(v, digits);
  const pct = (v, d = 1) => {
    const r = +v.toFixed(d);
    return (r > 0 ? '+' : r < 0 ? MINUS : '') + Math.abs(r).toFixed(d) + '%';
  };
  const short1 = x => (x >= 100 ? Math.round(x) : +x.toFixed(1)).toLocaleString('en-US');
  const compact = v => {
    const a = Math.abs(v), s = v < 0 ? MINUS : '';
    if (a >= 1e6) return s + '$' + short1(a / 1e6) + 'M';
    if (a >= 1e3) return s + '$' + short1(a / 1e3) + 'K';
    return s + '$' + Math.round(a);
  };
  const qty = q => q.toLocaleString('en-US', { maximumFractionDigits: 4 });
  const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));

  const day = s => new Date(s.slice(0, 10) + 'T00:00:00Z');
  const iso = d => d.toISOString().slice(0, 10);
  const addDays = (s, n) => iso(new Date(day(s).getTime() + n * 864e5));
  const fmtDate = (s, o) => day(s).toLocaleDateString('en-US', Object.assign({ timeZone: 'UTC' }, o));
  const shortDate = s => fmtDate(s, { month: 'short', day: 'numeric' });
  const longDate = s => fmtDate(s, { month: 'short', day: 'numeric', year: 'numeric' });
  const monthName = s => fmtDate(s, { month: 'long' });
  const rangeLabel = (from, to) => {
    if (from === to) return fmtDate(from, { month: 'long', day: 'numeric' });
    if (from.slice(0, 7) === to.slice(0, 7)) return monthName(from) + ' ' + day(from).getUTCDate() + '–' + day(to).getUTCDate();
    if (from.slice(0, 4) === to.slice(0, 4)) return shortDate(from) + ' – ' + shortDate(to);
    return longDate(from) + ' – ' + longDate(to);
  };

  const catName = c => c ? c.charAt(0) + c.slice(1).toLowerCase().replace(/_/g, ' ') : 'Uncategorized';
  const SUBTYPES = { ira: 'IRA', roth: 'Roth IRA', '401k': '401(k)', '403b': '403(b)', hsa: 'HSA', cd: 'CD', 'cash management': 'Cash management' };
  const typeLabel = a => {
    const s = (a.subtype || a.type || '').toLowerCase();
    if (SUBTYPES[s]) return SUBTYPES[s];
    return s ? s.charAt(0).toUpperCase() + s.slice(1) : 'Account';
  };
  const maskOf = m => m ? '••' + m : '';

  // A tidy axis: about `ticks` steps of 1, 2, 2.5 or 5 times a power of ten.
  function niceStep(span, ticks) {
    const rough = span / ticks, mag = 10 ** Math.floor(Math.log10(rough)), f = rough / mag;
    return (f <= 1 ? 1 : f <= 2 ? 2 : f <= 2.5 ? 2.5 : f <= 5 ? 5 : 10) * mag;
  }
  function axis(lo, hi, ticks = 4) {
    if (!(hi > lo)) { const pad = Math.abs(hi) * 0.05 || 100; lo -= pad; hi += pad; }
    const step = niceStep(hi - lo, ticks);
    const min = Math.floor(lo / step) * step, max = Math.ceil(hi / step) * step;
    const out = [];
    for (let v = min; v <= max + step / 2; v += step) out.push(+v.toFixed(6));
    return { min, max, step, ticks: out };
  }

  // --- talking to fin ---

  async function api(path, opts) {
    const res = await fetch(path, opts);
    let body = null;
    try { body = await res.json(); } catch (e) { /* not JSON */ }
    if (!res.ok) {
      const e = body && body.error;
      const err = new Error(e ? e.message : res.status + ' ' + res.statusText);
      err.code = e && e.code;
      throw err;
    }
    return body;
  }

  // --- the command in the header ---

  let copyText = '';
  const shq = s => '"' + s.replace(/(["\\$`])/g, '\\$1') + '"';
  // A flag value as the shell needs it: as is when it is one plain word.
  const arg = s => /^[\w.@%+:\/-]+$/.test(s) ? s : shq(s);
  function setCommand(args, full) {
    copyText = 'fin ' + (full || args) + ' --ui';
    put($('#cmd'), html`<span class="p">› </span><span class="f">fin</span> ${args} <span class="flag">--ui</span>`);
  }
  async function copy(text, btn) {
    try {
      await navigator.clipboard.writeText(text);
      btn.dataset.done = '';
      setTimeout(() => delete btn.dataset.done, 1200);
    } catch (e) { /* the clipboard can be refused; nothing to do */ }
  }
  $('#copy-cmd').addEventListener('click', e => copy(copyText, e.currentTarget));

  // --- pieces every page uses ---

  const ARROW_UP = html`<svg width="14" height="14" viewBox="0 0 24 24" aria-hidden="true"><path d="M7 17L17 7M8 7h9v9"></path></svg>`;
  const ARROW_DOWN = html`<svg width="14" height="14" viewBox="0 0 24 24" aria-hidden="true"><path d="M7 7l10 10M17 8v9H8"></path></svg>`;
  const CHEVRON = html`<svg width="14" height="14" viewBox="0 0 24 24" aria-hidden="true"><path d="M9 6l6 6-6 6"></path></svg>`;

  const figure = v => {
    const s = money2(v);
    return html`<div class="figure"><span class="whole">${s.slice(0, -3)}</span><span class="cents">${s.slice(-3)}</span></div>`;
  };
  // A change chip. warm says more spending or a loss; cool the opposite.
  const chip = (text, up, warm) => html`<span class="chip ${warm ? 'warm' : 'cool'}">${up == null ? '' : up ? ARROW_UP : ARROW_DOWN}${text}</span>`;
  const head = (n, title, right) => html`<div class="panel-head label"><h2><span class="n">${n}</span> / ${title}</h2>${right || ''}</div>`;
  const empty = (title, body) => html`<div class="empty"><span class="label">${title}</span><p>${body}</p></div>`;

  function spark(vals, w, h, marker) {
    if (!vals || vals.length < 2) return '';
    const lo = Math.min(...vals), hi = Math.max(...vals), n = vals.length;
    const pts = vals.map((v, i) => [i / (n - 1) * w, h - 4 - (v - lo) / (hi - lo || 1) * (h - 8)]);
    const d = pts.map((p, i) => (i ? 'L' : 'M') + p[0].toFixed(1) + ',' + p[1].toFixed(1)).join('');
    const last = pts[n - 1];
    return html`<svg class="spark" width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" aria-hidden="true"><path d="${d}"></path>${marker ? html`<rect x="${(last[0] - 3.5).toFixed(1)}" y="${(last[1] - 3.5).toFixed(1)}" width="7" height="7"></rect>` : ''}</svg>`;
  }

  const path = (vals, X, Y) => vals.map((v, i) => (i ? 'L' : 'M') + X(i).toFixed(1) + ',' + Y(v).toFixed(1)).join('');
  const edge = left => left > 92 ? '-100%' : left < 6 ? '0%' : '-50%';

  // Ticks for a daily series: month starts for long ones, weeks back from
  // the last day for short ones.
  function dayTicks(dates) {
    const n = dates.length;
    if (n < 2) return [];
    if (n > 45) {
      let starts = [];
      dates.forEach((s, i) => { if (s.endsWith('-01')) starts.push(i); });
      const step = Math.ceil(starts.length / 12);
      starts = starts.filter((_, k) => k % step === 0);
      const years = dates[0].slice(0, 4) !== dates[n - 1].slice(0, 4);
      return starts.map(i => ({ i, label: fmtDate(dates[i], { month: 'short' }) + (years && dates[i].slice(5, 7) === '01' ? ' ' + dates[i].slice(0, 4) : '') }));
    }
    const step = n > 14 ? 7 : n > 7 ? 2 : 1, out = [];
    for (let i = n - 1; i >= 0; i -= step) out.unshift({ i, label: shortDate(dates[i]) });
    return out;
  }

  function xAxis(ticks) {
    return html`<div class="xaxis" aria-hidden="true">${ticks.map(t => html`
      <div class="tick" style="left: ${t.left}%"></div>
      <div class="tl" style="left: ${t.left}%; transform: translateX(${t.tx || edge(+t.left)})">${t.label}</div>`)}</div>`;
  }

  // --- overview ---

  const GROUPS = { depository: 'Cash', credit: 'Credit cards', loan: 'Loans', investment: 'Investments', other: 'Other' };
  const RANGES = [['1M', 'in the last 30 days'], ['3M', 'in the last 3 months'], ['YTD', 'since January 1'], ['1Y', 'in the last 12 months'], ['ALL', '']];

  function rangeStart(range, today) {
    const d = day(today);
    switch (range) {
      case '1M': return addDays(today, -30);
      case '3M': d.setUTCMonth(d.getUTCMonth() - 3); return iso(d);
      case 'YTD': return today.slice(0, 4) + '-01-01';
      case '1Y': d.setUTCFullYear(d.getUTCFullYear() - 1); return iso(d);
    }
    return '';
  }

  async function overview() {
    setCommand('accounts');
    const d = await api('./api/overview');
    const hist = d.history;
    const groups = d.groups;
    const count = key => (groups.find(g => g.key === key) || { accounts: [] }).accounts.length;
    let range = hist.length > 120 ? 'YTD' : 'ALL';
    let pts = [];

    const months = d.months, last = months[months.length - 2];
    const kept = m => m.income - m.spent;
    const keptNote = last.income > 0
      ? Math.round(kept(last) / last.income * 100) + '% of income, after spending'
      : last.spent > 0 ? 'No income recorded; ' + money(last.spent) + ' spent' : 'No transactions that month';
    const cards = owedText => html`
      <section class="cards">
        <div class="panel hud">
          ${head('02', 'Assets')}
          <div class="card-body"><div class="card-fig">${money(d.assets)}</div>${spark(d.assets_history.slice(-120).map(p => p.value), 120, 36, true)}</div>
          <div class="card-note">${d.asset_accounts} ${d.asset_accounts === 1 ? 'account' : 'accounts'} in cash and investments</div>
        </div>
        <div class="panel hud">
          ${head('03', 'Liabilities')}
          <div class="card-body"><div class="card-fig">${money(d.liabilities)}</div>${spark(d.liabilities_history.slice(-120).map(p => p.value), 120, 36, true)}</div>
          <div class="card-note">${owedText}</div>
        </div>
        <div class="panel hud">
          ${head('04', 'Kept in ' + monthName(last.month + '-01'))}
          <div class="card-body"><div class="card-fig">${money(kept(last))}</div>${spark(months.slice(0, -1).map(kept), 120, 36, true)}</div>
          <div class="card-note">${keptNote}</div>
        </div>
      </section>`;
    const cardsN = count('credit'), loansN = count('loan');
    const owedText = cardsN + loansN === 0 ? 'No cards or loans'
      : [cardsN && cardsN + (cardsN === 1 ? ' card' : ' cards'), loansN && loansN + (loansN === 1 ? ' loan' : ' loans')].filter(Boolean).join(' and ');

    const itemsAt = {};
    groups.forEach(g => g.accounts.forEach(a => { (itemsAt[a.institution] = itemsAt[a.institution] || new Set()).add(a.item); }));
    const account = a => {
      let meta = '';
      if (a.type === 'credit' && a.limit > 0 && a.current != null) {
        const p = clamp(a.current / a.limit * 100, 0, 100);
        meta = html`<div class="meter-row"><div class="meter"><i style="width: ${p.toFixed(1)}%"></i></div><span>${Math.round(p)}% of ${money(a.limit)}</span></div>`;
      } else if (a.type === 'credit') {
        meta = 'No limit reported';
      } else if (a.type === 'depository' && a.available != null && a.current != null && Math.abs(a.available - a.current) >= 0.01) {
        meta = 'Available ' + money2(a.available);
      }
      return html`
        <div class="acct">
          <div class="who">
            <div class="tile" aria-hidden="true">${(a.institution || a.item || '?').charAt(0).toUpperCase()}</div>
            <div style="min-width: 0">
              <div class="name">${a.name}</div>
              <div class="sub">${a.institution || a.item}${a.institution && itemsAt[a.institution].size > 1 ? ' · ' + a.item : ''} · ${typeLabel(a)}</div>
            </div>
          </div>
          <div class="mask">${maskOf(a.mask)}</div>
          <div class="meta">${meta}</div>
          ${spark(a.history, 96, 28, false)}
          <div class="bal">${a.balance == null ? '—' : money2(a.balance)}</div>
        </div>`;
    };

    put(main, html`
      <section class="hero">
        <div id="nw-hero"></div>
        <div id="nw-ranges"></div>
      </section>
      <section class="panel hud" aria-label="Net worth over time">
        ${head('01', 'Net worth', html`<span id="nw-sub"></span>`)}
        <div id="nw-chart"></div>
      </section>
      ${cards(owedText)}
      <section class="panel hud mt" aria-label="Accounts">
        ${head('05', 'Accounts', html`<span>${d.accounts} ${d.accounts === 1 ? 'account' : 'accounts'} · ${d.items} ${d.items === 1 ? 'connection' : 'connections'}</span>`)}
        <div class="accts">
          ${groups.length ? groups.map(g => html`
            <div class="group">
              <div class="group-head"><span>${GROUPS[g.key] || g.key}</span><span class="num">${money2(g.total)}</span></div>
              ${g.accounts.map(account)}
            </div>`) : empty('No accounts yet', html`Connect a bank with <code>fin link</code>, then run <code>fin sync</code>.`)}
        </div>
      </section>`);

    const hero = idx => {
      const has = pts.length >= 2;
      const i = idx == null ? pts.length - 1 : idx;
      // Today is the accounts' own total, the one the cards and the list add
      // up to; the history only answers for the days you hover.
      const v = has && idx != null ? pts[i].value : d.net_worth;
      const when = idx == null ? 'Today, ' + shortDate(d.today) : longDate(pts[i].date);
      let deltas;
      if (has) {
        const delta = v - pts[0].value, rel = pts[0].value ? delta / Math.abs(pts[0].value) * 100 : 0;
        const since = range === 'ALL' ? 'since ' + longDate(pts[0].date) : RANGES.find(r => r[0] === range)[1];
        const flat = Math.abs(delta) < 0.5;
        deltas = html`${chip(signed(delta) + '  ' + pct(rel), flat ? null : delta > 0, !flat && delta < 0)}<span class="caps">${idx == null ? since : 'from ' + longDate(pts[0].date)}</span>`;
      } else {
        deltas = html`<span class="caps">${money(d.assets)} in assets, ${money(d.liabilities)} owed</span>`;
      }
      put($('#nw-hero'), html`
        <div class="kicker"><span class="sq">■</span> Net worth · ${when}</div>
        ${figure(v)}
        <div class="deltas">${deltas}</div>`);
    };

    const chart = () => {
      const start = rangeStart(range, d.today);
      pts = hist.filter(p => p.date >= start);
      put($('#nw-sub'), pts.length >= 2 ? 'Daily close · USD · ' + (range === 'ALL' ? 'since ' + shortDate(pts[0].date) : range) : '');
      put($('#nw-ranges'), hist.length >= 2 ? html`<div class="seg" role="group" aria-label="Range">${RANGES.map(([r]) =>
        html`<button type="button" data-range="${r}" aria-pressed="${r === range}">${r}</button>`)}</div>` : '');
      hero(null);
      const el = $('#nw-chart');
      if (pts.length < 2) {
        put(el, empty(hist.length ? 'One day so far' : 'No history yet', html`fin keeps one balance per account for each day it syncs${d.history_since ? ', starting ' + longDate(d.history_since) : ''}. Run <code>fin sync</code> on other days, or let <code>fin transactions</code> and <code>fin holdings</code> sync for you, and this line fills in.`));
        return;
      }
      const n = pts.length, vals = pts.map(p => p.value);
      const lo = Math.min(...vals), hi = Math.max(...vals), span = hi - lo || Math.abs(hi) * 0.02 || 100;
      const ax = axis(lo - span * 0.12, hi + span * 0.1, 4);
      const X = i => i / (n - 1) * 1000, Y = v => 300 - (v - ax.min) / (ax.max - ax.min) * 300;
      const line = path(vals, X, Y);
      const ticks = dayTicks(pts.map(p => p.date)).map(t => ({ left: (t.i / (n - 1) * 100).toFixed(2), label: t.label }));
      put(el, html`
        <div class="plot-pad y-left">
          <div class="plot" id="nw-plot">
            ${ticks.map(t => html`<div class="vgrid" style="left: ${t.left}%"></div>`)}
            ${ax.ticks.map((v, k) => html`<div class="hrule${k ? '' : ' base'}" style="top: ${(Y(v) / 3).toFixed(2)}%"></div><div class="ylab out" style="top: ${(Y(v) / 3).toFixed(2)}%">${compact(v)}</div>`)}
            <svg class="lines" viewBox="0 0 1000 300" preserveAspectRatio="none" aria-hidden="true">
              <defs><linearGradient id="nwWash" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#4A96E8" stop-opacity="0.26"></stop><stop offset="1" stop-color="#4A96E8" stop-opacity="0"></stop></linearGradient></defs>
              <path d="${line + 'L1000,300L0,300Z'}" fill="url(#nwWash)"></path>
              <path d="${line}" fill="none" stroke="#4A96E8" stroke-width="2" stroke-linejoin="miter" stroke-linecap="square"></path>
            </svg>
            <div id="nw-cursor" hidden>
              <div class="cur-v"></div><div class="cur-h"></div><div class="cur-tag"></div><div class="cur-val"></div>
            </div>
            <div class="dot" id="nw-dot"></div>
            <div class="hit" id="nw-hit"></div>
          </div>
          ${xAxis(ticks)}
        </div>`);
      const cur = $('#nw-cursor'), dot = $('#nw-dot');
      const move = idx => {
        const i = idx == null ? n - 1 : idx, left = X(i) / 10, top = Y(vals[i]) / 3;
        dot.style.left = left + '%'; dot.style.top = top + '%';
        cur.hidden = idx == null;
        if (idx != null) {
          $('.cur-v', cur).style.left = left + '%';
          $('.cur-h', cur).style.top = top + '%';
          const tag = $('.cur-tag', cur);
          tag.textContent = longDate(pts[i].date);
          tag.style.left = left + '%';
          tag.style.transform = 'translateX(' + (left > 90 ? '-100%' : left < 10 ? '0%' : '-50%') + ')';
          const cv = $('.cur-val', cur);
          cv.textContent = money(vals[i]);
          cv.style.top = top + '%';
        }
        hero(idx);
      };
      move(null);
      const hit = $('#nw-hit');
      let last = null;
      hit.addEventListener('mousemove', e => {
        const r = hit.getBoundingClientRect();
        const i = Math.round(clamp((e.clientX - r.left) / r.width, 0, 1) * (n - 1));
        if (i !== last) { last = i; move(i); }
      });
      hit.addEventListener('mouseleave', () => { last = null; move(null); });
    };
    $('#nw-ranges').addEventListener('click', e => {
      const b = e.target.closest('[data-range]');
      if (b) { range = b.dataset.range; chart(); }
    });
    chart();
  }

  // --- spending ---

  const RAMP = ['#18304B', '#1F4670', '#2A5F99', '#3A7DC4', '#6AA8EC'];
  const PRESETS = [['Month', t => t.slice(0, 8) + '01'], ['30D', t => addDays(t, -29)], ['90D', t => addDays(t, -89)], ['YTD', t => t.slice(0, 4) + '-01-01']];

  async function spending() {
    const q = new URLSearchParams(location.search);
    const d = await api('./api/spending?' + q);
    const P = d.period, C = d.compare, mtd = d.month_to_date;
    const args = 'transactions --since ' + P.from + (q.get('until') ? ' --until ' + P.to : '') + (d.account ? ' --account ' + arg(d.account) : '');
    setCommand(args);

    const cum = xs => { let s = 0; return xs.map(x => (s += x)); };
    const co = cum(d.daily), cs = cum(d.compare_daily);
    const A = Math.max(d.axis_days, C.days, P.days);
    const delta = d.spent - d.compare_to_date;
    const curName = mtd ? monthName(P.from) : 'This period';
    const cmpName = mtd ? monthName(C.from) : 'Before';
    const vsText = mtd ? 'vs. this day in ' + monthName(C.from) : 'vs. the ' + C.days + ' days before';
    const cmpRange = rangeLabel(C.from, addDays(C.from, Math.min(P.days, C.days) - 1));

    // Every day of the month the period ends in, with what was spent.
    const byDate = {};
    d.daily.forEach((v, i) => { byDate[addDays(P.from, i)] = { v, n: d.daily_count[i] }; });
    const spendTx = d.transactions.filter(t => t.spending);
    const monthStart = P.to.slice(0, 8) + '01';
    const monthDays = new Date(Date.UTC(+P.to.slice(0, 4), +P.to.slice(5, 7), 0)).getUTCDate();
    const lead = (day(monthStart).getUTCDay() + 6) % 7;
    const maxDay = Math.max(0, ...Object.keys(byDate).filter(k => k.startsWith(P.to.slice(0, 8))).map(k => byDate[k].v));
    const bucket = v => v <= 0 || !maxDay ? 0 : Math.min(4, Math.floor(Math.sqrt(v / maxDay) * 5));
    let sel = P.to;

    const presetFor = PRESETS.find(([, f]) => !q.get('until') && (q.get('since') || d.today.slice(0, 8) + '01') === f(d.today)) || [''];
    const presetLink = ([name, f]) => {
      const p = new URLSearchParams();
      p.set('since', f(d.today));
      if (d.account) p.set('account', d.account);
      const s = p.toString();
      return html`<a href="./spending${s ? '?' + s : ''}" aria-current="${presetFor[0] === name}">${name}</a>`;
    };

    const cats = (() => {
      const pos = d.categories.filter(c => c.spent > 0);
      const top = pos.slice(0, 7), rest = pos.slice(7);
      if (rest.length) top.push({ category: null, label: 'Everything else', spent: rest.reduce((a, c) => a + c.spent, 0), compare: rest.reduce((a, c) => a + c.compare, 0) });
      const maxC = Math.max(1, ...top.map(c => c.spent));
      return top.map(c => {
        const change = c.compare > 0 ? c.spent / c.compare - 1 : null;
        const whole = change == null ? 0 : Math.round(Math.abs(change) * 100);
        return {
          name: c.label || catName(c.category), amount: money(c.spent), pct: (c.spent / maxC * 100).toFixed(1),
          delta: change == null ? 'NEW' : whole === 0 ? '0%' : (change > 0 ? '+' : MINUS) + whole + '%',
          cls: change == null ? 'new' : change > 0.005 ? 'up' : change < -0.005 ? 'down' : 'flat',
        };
      });
    })();

    put(main, html`
      <section class="hero">
        <div>
          <div class="kicker"><span class="sq">■</span> Spent · ${rangeLabel(P.from, P.to)}${mtd ? ' · month to date' : ''}${d.account ? ' · ' + d.account : ''}</div>
          ${figure(d.spent)}
          <div class="deltas">
            ${Math.abs(delta) >= 0.5 ? chip(signed(delta), delta > 0, delta > 0) : ''}
            <span class="caps">${Math.abs(delta) >= 0.5 ? vsText : vsText.replace('vs.', 'same as')}</span>
          </div>
        </div>
        <nav class="seg" aria-label="Period">${PRESETS.map(presetLink)}</nav>
      </section>

      <section class="row">
        <div class="panel hud grow-2">
          ${head('01', 'Running total', html`<div class="legend"><span><i class="key" style="background: #4A96E8"></i>${curName}</span><span><i class="key" style="background: #66768A"></i>${cmpName}</span></div>`)}
          <div class="plot-pad y-left"><div class="plot short" id="pace"></div><div id="pace-axis"></div></div>
        </div>
        <div class="panel hud grow-1">
          ${head('02', 'Every day', html`<span>${fmtDate(monthStart, { month: 'short', year: 'numeric' })}</span>`)}
          <div class="cal-wrap">
            <div class="cal dow" aria-hidden="true"><span>MON</span><span>TUE</span><span>WED</span><span>THU</span><span>FRI</span><span>SAT</span><span>SUN</span></div>
            <div class="cal days" id="cal"></div>
            <div class="ramp" aria-hidden="true"><span>Less</span><div>${RAMP.map(c => html`<i style="background: ${c}"></i>`)}</div><span>More</span></div>
            <div class="day-card" id="day-card"></div>
          </div>
        </div>
      </section>

      <section class="row top mt">
        <div class="panel hud" style="flex: 1 1 360px">
          ${head('03', 'Where it went', html`<span>vs. ${cmpRange}</span>`)}
          <div class="cats">${cats.length ? cats.map(c => html`
            <div>
              <div class="cat-line"><span class="nm">${c.name}</span><span class="am">${c.amount}</span><span class="dl ${c.cls}">${c.delta}</span></div>
              <div class="bar-track"><i style="width: ${c.pct}%"></i></div>
            </div>`) : html`<p class="label">Nothing spent in this period</p>`}</div>
        </div>
        <div class="panel hud" style="flex: 999 1 600px">
          ${head('04', 'Transactions', html`<span>Amazon and Costco open into items</span>`)}
          <div id="txs"></div>
        </div>
      </section>`);

    // Running total, this period against the one before.
    const ymax = axis(Math.min(0, ...co, ...cs), Math.max(1, ...co, ...cs), 4);
    const X = i => A > 1 ? i / (A - 1) * 1000 : 500, Y = v => 300 - (v - ymax.min) / (ymax.max - ymax.min) * 300;
    const curLine = path(co, X, Y), cmpLine = path(cs, X, Y);
    const xt = [];
    if (mtd) {
      for (let dd = 1; dd <= A; dd += 5) xt.push({ i: dd - 1, label: fmtDate(P.from, { month: 'short' }) + ' ' + dd });
    } else {
      const step = Math.max(1, Math.ceil((A - 1) / 6));
      for (let i = 0; i < A; i += step) xt.push({ i, label: shortDate(addDays(P.from, i)) });
    }
    const ticks = xt.map(t => ({ left: (X(t.i) / 10).toFixed(2), label: t.label, tx: '-50%' }));
    const cmpEnd = cs.length ? cs[cs.length - 1] : 0;
    put($('#pace'), html`
      ${ticks.map(t => html`<div class="vgrid" style="left: ${t.left}%"></div>`)}
      ${ymax.ticks.map((v, k) => html`<div class="hrule${k ? '' : ' base'}" style="top: ${(Y(v) / 3).toFixed(2)}%"></div><div class="ylab out" style="top: ${(Y(v) / 3).toFixed(2)}%">${compact(v)}</div>`)}
      <svg class="lines" viewBox="0 0 1000 300" preserveAspectRatio="none" aria-hidden="true">
        <defs><linearGradient id="paceWash" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#4A96E8" stop-opacity="0.24"></stop><stop offset="1" stop-color="#4A96E8" stop-opacity="0"></stop></linearGradient></defs>
        ${cs.length > 1 ? html`<path d="${cmpLine}" fill="none" stroke="#66768A" stroke-width="1.5"></path>` : ''}
        ${co.length > 1 ? html`<path d="${curLine + 'L' + X(co.length - 1).toFixed(1) + ',300L0,300Z'}" fill="url(#paceWash)"></path><path d="${curLine}" fill="none" stroke="#4A96E8" stroke-width="2"></path>` : ''}
      </svg>
      ${cs.length ? html`<div class="end-tag" style="top: calc(${(Y(cmpEnd) / 3).toFixed(2)}% - 24px)">${cmpName} total ${money(cmpEnd)}</div>` : ''}
      <div id="pace-hover"></div>
      ${co.length ? html`<div class="dot" style="left: ${(X(co.length - 1) / 10).toFixed(2)}%; top: ${(Y(co[co.length - 1]) / 3).toFixed(2)}%"></div>` : ''}
      <div class="hit" id="pace-hit" style="inset: 0"></div>`);
    put($('#pace-axis'), xAxis(ticks));
    const paceHover = i => {
      if (i == null) { put($('#pace-hover'), ''); return; }
      const left = X(i) / 10, right = left > 62;
      const curV = i < co.length ? money(co[i]) : '—', cmpV = i < cs.length ? money(cs[i]) : '—';
      const label = mtd ? 'Day ' + (i + 1) : 'Day ' + (i + 1) + ' · ' + shortDate(addDays(P.from, i));
      put($('#pace-hover'), html`
        <div class="cur-v" style="top: 0; left: ${left}%"></div>
        ${i < cs.length ? html`<div class="dot small" style="left: ${left}%; top: ${(Y(cs[i]) / 3).toFixed(2)}%"></div>` : ''}
        <div class="tip" style="left: ${left}%; transform: translateX(${right ? '-100%' : '0%'}); margin-left: ${right ? '-12px' : '12px'}">
          <div class="t">${label}</div>
          <div class="r"><span><i class="sw" style="background: #4A96E8"></i>${curName}</span><span>${curV}</span></div>
          <div class="r"><span><i class="sw" style="background: #66768A"></i>${cmpName}</span><span>${cmpV}</span></div>
        </div>`);
    };
    const hit = $('#pace-hit');
    let lastI = null;
    hit.addEventListener('mousemove', e => {
      const r = hit.getBoundingClientRect();
      const i = Math.round(clamp((e.clientX - r.left) / r.width, 0, 1) * (A - 1));
      if (i !== lastI) { lastI = i; paceHover(i); }
    });
    hit.addEventListener('mouseleave', () => { lastI = null; paceHover(null); });

    // The month as a heat grid.
    const cells = [];
    for (let k = 0; k < lead; k++) cells.push(html`<div class="blank" aria-hidden="true"></div>`);
    for (let n = 1; n <= monthDays; n++) {
      const date = monthStart.slice(0, 8) + String(n).padStart(2, '0');
      const v = byDate[date];
      const name = fmtDate(date, { weekday: 'long', month: 'long', day: 'numeric' });
      if (!v) {
        cells.push(html`<div class="cell off" aria-label="${name}: outside this period">${String(n).padStart(2, '0')}</div>`);
        continue;
      }
      const b = bucket(v.v);
      cells.push(html`<button type="button" class="cell" data-date="${date}" aria-pressed="${date === sel}" aria-label="${name}: ${money(v.v)}" style="background: ${RAMP[b]}; color: ${b >= 4 ? '#06080C' : '#C9D2DC'}">${String(n).padStart(2, '0')}</button>`);
    }
    while (cells.length % 7) cells.push(html`<div class="blank" aria-hidden="true"></div>`);
    put($('#cal'), cells);
    const dayCard = () => {
      const v = byDate[sel] || { v: 0, n: 0 };
      const top = spendTx.filter(t => t.date === sel).sort((a, b) => b.amount - a.amount)[0];
      const detail = v.n + (v.n === 1 ? ' transaction' : ' transactions') + (top && v.n > 1 ? ' · ' + (top.merchant_name || top.name) + ' ' + money(top.amount) : '');
      put($('#day-card'), html`<div>
        <div class="d">${fmtDate(sel, { weekday: 'short', month: 'short', day: 'numeric' })}${sel === d.today ? ' · today' : ''}</div>
        <div class="v"><span>${money(v.v)}</span><span>${detail}</span></div>
      </div>`);
    };
    dayCard();
    const pick = e => {
      const c = e.target.closest('.cell[data-date]');
      if (!c || c.dataset.date === sel) return;
      sel = c.dataset.date;
      $$('.cell[data-date]').forEach(x => x.setAttribute('aria-pressed', String(x.dataset.date === sel)));
      dayCard();
    };
    $('#cal').addEventListener('mouseover', pick);
    $('#cal').addEventListener('focusin', pick);

    // Transactions, with the retailer items each paid for.
    let shown = 25;
    const open = new Set();
    const RETAILERS = { amazon: 'Amazon', costco: 'Costco' };
    const txRow = t => {
      const items = t.items || [];
      const id = t.transaction_id;
      const isOpen = open.has(id);
      const amt = t.amount < 0 ? '+' + money2(-t.amount) : money2(t.amount);
      const cls = t.amount < 0 ? 'in' : t.spending ? '' : 'muted';
      const sub = catName(t.category) + (items.length ? ' · ' + items.length + (items.length === 1 ? ' item' : ' items') : '') + (t.spending ? '' : ' · not counted');
      const orders = [...new Set(items.map(it => RETAILERS[it.retailer] ? RETAILERS[it.retailer] + ' ' + it.order_id : it.order_id))];
      return html`
        <div class="tx">
          <div class="trow">
            <span class="c-date">${shortDate(t.date)}</span>
            <div class="c-main">
              <div class="m"><span>${t.merchant_name || t.name}</span>${t.pending ? html`<span class="tag">Pending</span>` : ''}</div>
              <div class="s">${sub}</div>
            </div>
            <span class="c-acct">${t.account_name} ${maskOf(t.account_mask)}</span>
            <span class="c-amt ${cls}">${amt}</span>
            <span class="c-btn">${items.length ? html`<button type="button" class="toggle" data-tx="${id}" aria-expanded="${isOpen}" aria-label="${(isOpen ? 'Hide' : 'Show') + ' items in this ' + (t.merchant_name || t.name) + ' charge'}">${CHEVRON}</button>` : ''}</span>
          </div>
          ${isOpen ? html`<div class="items">
            <div class="t">${orders.join(' · ')} · cost includes its share of tax and shipping</div>
            ${items.map(it => html`<div class="item"><span class="ti">${it.quantity > 1 ? qty(it.quantity) + ' × ' : ''}${it.title}</span><span class="ca">${it.category ? catName(it.category) : ''}</span><span class="co">${money2(it.cost)}</span></div>`)}
          </div>` : ''}
        </div>`;
    };
    const txs = () => {
      const all = d.transactions;
      put($('#txs'), all.length ? html`
        <div class="scroll"><div style="min-width: 600px">
          <div class="thead"><span class="c-date">Date</span><span class="c-main">Merchant</span><span class="c-acct">Account</span><span class="c-amt">Amount</span><span class="c-btn"></span></div>
          ${all.slice(0, shown).map(txRow)}
        </div></div>
        <div class="panel-foot">
          <span class="caps">Showing ${Math.min(shown, all.length)} of ${all.length}</span>
          ${shown < all.length ? html`<button type="button" class="btn" id="more">Show all ${all.length}</button>` : ''}
          <code>fin ${args} --json</code>
        </div>` : html`<div class="empty"><span class="label">No transactions</span><p>Nothing in ${rangeLabel(P.from, P.to)}. Run <code>fin sync</code> to bring in new ones.</p></div>`);
    };
    txs();
    $('#txs').addEventListener('click', e => {
      const b = e.target.closest('[data-tx]');
      if (b) {
        const id = b.dataset.tx;
        open.has(id) ? open.delete(id) : open.add(id);
        txs();
        const again = $('[data-tx="' + CSS.escape(id) + '"]');
        if (again) again.focus();
      } else if (e.target.closest('#more')) {
        shown = Infinity;
        txs();
      }
    });
  }

  // --- portfolio ---

  const CLASSES = [
    ['stocks', 'Stocks', '#4A96E8'], ['funds', 'Funds', '#E0692F'], ['bonds', 'Bonds', '#1FA97A'],
    ['cash', 'Cash', '#C98500'], ['crypto', 'Crypto', '#D75D8C'], ['other', 'Other', '#5B6675'],
  ];
  const classOf = key => CLASSES.find(c => c[0] === key) || CLASSES[5];
  const gainFill = g => g == null ? '#2B333E' : g >= 35 ? '#2E6FC0' : g >= 15 ? '#285A99' : g > 0.5 ? '#1F3F66' : g >= -0.5 ? '#2B333E' : g > -5 ? '#4E2E24' : g > -15 ? '#7E4026' : '#B5552A';
  const gainInk = g => g == null ? '#AEB8C4' : g > 0.5 ? '#8CC0F4' : g < -0.5 ? '#F2A27A' : '#AEB8C4';

  // Squarified treemap: rows of tiles kept as close to square as they fit.
  function squarify(items, W, H) {
    const out = [], r = { x: 0, y: 0, w: W, h: H };
    const sum = row => row.reduce((a, b) => a + b.area, 0);
    const worst = (row, len) => {
      const s = sum(row);
      let mx = 0, mn = Infinity;
      row.forEach(it => { mx = Math.max(mx, it.area); mn = Math.min(mn, it.area); });
      return Math.max(len * len * mx / (s * s), (s * s) / (len * len * mn));
    };
    const place = row => {
      const s = sum(row);
      if (r.w >= r.h) {
        const cw = s / r.h;
        let yy = r.y;
        row.forEach(it => { const hh = it.area / cw; out.push(Object.assign({}, it, { x: r.x, y: yy, w: cw, h: hh })); yy += hh; });
        r.x += cw; r.w -= cw;
      } else {
        const rh = s / r.w;
        let xx = r.x;
        row.forEach(it => { const ww = it.area / rh; out.push(Object.assign({}, it, { x: xx, y: r.y, w: ww, h: rh })); xx += ww; });
        r.y += rh; r.h -= rh;
      }
    };
    let row = [], i = 0;
    while (i < items.length) {
      const len = Math.min(r.w, r.h);
      if (!row.length || worst(row.concat([items[i]]), len) <= worst(row, len)) { row.push(items[i]); i++; } else { place(row); row = []; }
    }
    if (row.length) place(row);
    return out;
  }

  async function portfolio() {
    const q = new URLSearchParams(location.search);
    const d = await api('./api/portfolio?' + q);
    setCommand('holdings' + (d.account ? ' --account ' + arg(d.account) : ''));
    if (!d.holdings.length) {
      put(main, html`<section class="hero"><div><div class="kicker"><span class="sq">■</span> Invested</div>${figure(0)}</div></section>
        <section class="panel hud">${head('01', 'Holdings')}${empty('No holdings yet', html`Connect a brokerage with <code>fin link</code>, then run <code>fin holdings</code> or <code>fin sync</code>.`)}</section>`);
      return;
    }
    const hs = d.holdings.map((h, k) => {
      const gain = h.cost_basis != null ? h.value - h.cost_basis : null;
      return Object.assign({}, h, {
        key: String(k),
        label: h.ticker || ((n => n.length > 12 ? n.slice(0, 11).trim() + '…' : n)(h.security_name || '—')),
        gain, g: gain != null && h.cost_basis > 0 ? gain / h.cost_basis * 100 : null,
      });
    });
    const total = d.total;
    const gain = d.basis_value - d.cost_basis;
    let sel = hs[0].key;

    const byClass = CLASSES.map(c => ({ c, v: hs.filter(h => h.class === c[0]).reduce((a, h) => a + h.value, 0) })).filter(x => x.v > 0);
    const W = 1000, H = 460;
    const positive = hs.filter(h => h.value > 0), area = positive.reduce((a, h) => a + h.value, 0);
    const laid = squarify(positive.map(h => Object.assign({}, h, { area: h.value / area * W * H })), W, H);
    const gainText = h => h.gain == null ? 'No cost basis' : signed(h.gain, 2) + '  ' + pct(h.g);
    const maxV = Math.max(...hs.map(h => h.value));

    put(main, html`
      <section class="hero">
        <div>
          <div class="kicker"><span class="sq">■</span> Invested · ${d.accounts} ${d.accounts === 1 ? 'account' : 'accounts'} · ${hs.length} ${hs.length === 1 ? 'holding' : 'holdings'}${d.account ? ' · ' + d.account : ''}</div>
          ${figure(total)}
          <div class="deltas">
            ${d.cost_basis > 0 ? html`${chip(signed(gain) + '  ' + pct(gain / d.cost_basis * 100), gain >= 0, gain < 0)}<span class="caps">Unrealized on ${money(d.cost_basis)} cost basis${d.basis_value < total - 0.5 ? ', where known' : ''}</span>` : html`<span class="caps">No cost basis reported</span>`}
            ${d.previous ? html`<span class="slash" aria-hidden="true">/</span><span class="caps light">${signed(total - d.previous.value)} since the ${shortDate(d.previous.date)} snapshot</span>` : ''}
          </div>
        </div>
      </section>

      <section class="panel hud" aria-label="Allocation">
        ${head('01', 'Allocation', html`<span>By asset class · as of ${longDate(d.as_of)}</span>`)}
        <div class="alloc">
          <div class="alloc-bar" aria-hidden="true">${byClass.map(x => html`<i style="flex: ${(x.v / total * 1000).toFixed(1)} 1 0; background: ${x.c[2]}"></i>`)}</div>
          <div class="ruler" aria-hidden="true"></div>
          <div class="alloc-grid">${byClass.map(x => html`
            <div><i class="sw" style="background: ${x.c[2]}" aria-hidden="true"></i><div>
              <div class="nm">${x.c[1]}</div>
              <div class="pc">${(x.v / total * 100).toFixed(1)}%</div>
              <div class="vl">${money2(x.v)}</div>
            </div></div>`)}</div>
        </div>
      </section>

      <section class="panel hud mt" aria-label="Positions">
        ${head('02', 'Positions · area = value', html`<div class="gain-scale"><span>${MINUS}15%</span><div aria-hidden="true">${['#B5552A', '#7E4026', '#4E2E24', '#2B333E', '#1F3F66', '#285A99', '#2E6FC0'].map(c => html`<i style="background: ${c}"></i>`)}</div><span>+35% gain</span></div>`)}
        <div class="tree-row">
          <div class="tree" id="tree">${laid.map(t => {
            const tw = t.w, th = t.h;
            return html`<button type="button" class="tile-btn${tw < 60 || th < 44 ? ' tiny' : ''}" data-key="${t.key}" aria-pressed="${t.key === sel}" aria-label="${t.label}, ${money(t.value)}, ${t.g == null ? 'no cost basis' : pct(t.g)}"
              style="left: calc(${(t.x / W * 100).toFixed(3)}% + 1px); top: calc(${(t.y / H * 100).toFixed(3)}% + 1px); width: calc(${(tw / W * 100).toFixed(3)}% - 2px); height: calc(${(th / H * 100).toFixed(3)}% - 2px); background: ${gainFill(t.g)}">
              ${tw > 52 && th > 34 ? html`<span class="tk" style="font-size: ${tw > 300 && th > 200 ? 22 : 13}px">${t.label}</span>` : ''}
              ${tw > 96 && th > 92 ? html`<span class="dt"><span>${compact(t.value)}</span><span>${t.g == null ? '—' : pct(t.g)}</span></span>` : ''}
            </button>`;
          })}</div>
          <div class="detail" id="detail"></div>
        </div>
      </section>

      <section class="panel hud mt" aria-label="Holdings">
        ${head('03', 'Holdings', html`<code>fin holdings${d.account ? ' --account ' + arg(d.account) : ''} --json</code>`)}
        <div class="scroll" style="padding-bottom: 8px"><div style="min-width: 900px" id="hrows" role="listbox" aria-label="Holdings">
          <div class="thead"><span class="h-main" style="padding-left: 8px">Holding</span><span class="h-acct">Account</span><span class="h-n">Shares</span><span class="h-n">Price</span><span class="h-v">Value</span><span class="h-w">Weight</span><span class="h-g">Gain</span></div>
          ${hs.map(h => html`
            <div class="hrow" role="option" tabindex="0" data-key="${h.key}" aria-selected="${h.key === sel}">
              <div class="h-main"><i class="sw" style="background: ${classOf(h.class)[2]}" aria-hidden="true"></i><span class="tk">${h.label}</span><span class="nm">${h.security_name || ''}</span></div>
              <span class="h-acct">${h.account_name} · ${h.item}</span>
              <span class="h-n">${qty(h.quantity)}</span>
              <span class="h-n">${money2(h.price)}</span>
              <span class="h-v">${money2(h.value)}</span>
              <span class="h-w"><span class="tr"><i style="width: ${(h.value / maxV * 100).toFixed(1)}%"></i></span><span>${(h.value / total * 100).toFixed(1)}%</span></span>
              <span class="h-g" style="color: ${gainInk(h.g)}">${h.gain == null ? '—' : Math.abs(h.gain) < 0.005 ? '—' : signed(h.gain) + '  ' + pct(h.g)}</span>
            </div>`)}
        </div></div>
      </section>`);

    const detail = () => {
      const h = hs.find(x => x.key === sel);
      const c = classOf(h.class);
      put($('#detail'), html`
        <div class="cls"><i class="sw" style="background: ${c[2]}" aria-hidden="true"></i><span>${c[1]}</span></div>
        <div class="tk">${h.label}</div>
        <div class="nm">${h.security_name || ''}</div>
        <div class="ac">${h.account_name} · ${h.institution || h.item}</div>
        <div class="vl">${money2(h.value)}</div>
        <div class="gn" style="color: ${gainInk(h.g)}">${gainText(h)}</div>
        <div class="facts">
          <div><span>Shares</span><span>${qty(h.quantity)}</span></div>
          <div><span>Price</span><span>${money2(h.price)}${h.price_as_of ? ' · ' + (h.price_as_of.slice(0, 4) === d.as_of.slice(0, 4) ? shortDate(h.price_as_of) : longDate(h.price_as_of)) : ''}</span></div>
          <div><span>Cost basis</span><span>${h.cost_basis == null ? '—' : money2(h.cost_basis)}</span></div>
          <div><span>Weight</span><span>${(h.value / total * 100).toFixed(1)}%</span></div>
        </div>`);
    };
    const select = key => {
      if (!key || key === sel) return;
      sel = key;
      $$('#tree .tile-btn').forEach(b => b.setAttribute('aria-pressed', String(b.dataset.key === sel)));
      $$('#hrows .hrow').forEach(r => r.setAttribute('aria-selected', String(r.dataset.key === sel)));
      detail();
    };
    detail();
    const onTile = e => { const b = e.target.closest('[data-key]'); if (b) select(b.dataset.key); };
    $('#tree').addEventListener('mouseover', onTile);
    $('#tree').addEventListener('focusin', onTile);
    $('#tree').addEventListener('click', onTile);
    $('#hrows').addEventListener('click', onTile);
    $('#hrows').addEventListener('keydown', e => {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onTile(e); }
    });
  }

  // --- query ---

  const DEFAULT_SQL = `select date_trunc('month', date)::date as month,
       category,
       sum(amount) as spent
from   transactions
where  amount > 0
  and  category not in ('TRANSFER_OUT', 'LOAN_PAYMENTS')
  and  date >= date_trunc('month', current_date) - interval 11 month
group by 1, 2
order by 1, 2`;

  const KEYWORDS = new Set(('select from where and or not in as group by order limit offset join left right inner outer full cross on using with having ' +
    'case when then else end distinct union all asc desc interval between like ilike is null true false over partition filter qualify window ' +
    'exists cast values describe show summarize pivot unpivot except intersect recursive lateral asof natural').split(' '));
  function highlight(src) {
    const re = /(--[^\n]*)|('(?:[^']|'')*'?)|("(?:[^"]|"")*"?)|(\b\d+(?:\.\d+)?\b)|([A-Za-z_][A-Za-z0-9_]*)|(\s+)|([^\sA-Za-z0-9_'"]+)/g;
    let out = '', m;
    while ((m = re.exec(src))) {
      const [, cm, str, quoted, num, word, ws, pu] = m;
      if (cm) out += '<span class="hl-c">' + esc(cm) + '</span>';
      else if (str) out += '<span class="hl-s">' + esc(str) + '</span>';
      else if (quoted) out += esc(quoted);
      else if (num) out += '<span class="hl-n">' + esc(num) + '</span>';
      else if (word) {
        if (KEYWORDS.has(word.toLowerCase())) out += '<span class="hl-k">' + esc(word) + '</span>';
        else if (/^\s*\(/.test(src.slice(re.lastIndex))) out += '<span class="hl-f">' + esc(word) + '</span>';
        else out += esc(word);
      } else if (ws) out += ws;
      else out += '<span class="hl-p">' + esc(pu) + '</span>';
    }
    return out;
  }

  const SERIES = ['#4A96E8', '#E0692F', '#1FA97A', '#C98500', '#D75D8C'];
  const OTHER = '#5B6675';
  const isNum = t => /^(DECIMAL|DOUBLE|FLOAT|REAL|HUGEINT|UHUGEINT|BIGINT|UBIGINT|INTEGER|UINTEGER|SMALLINT|USMALLINT|TINYINT|UTINYINT|INT)/.test(t || '');
  const isTime = t => /^(DATE|TIMESTAMP)/.test(t || '');
  const isText = t => /^(VARCHAR|TEXT|STRING|ENUM)/.test(t || '');

  // analyze picks a chart for a result: dates and a category stack up over
  // time, dates alone draw lines, a text column lines up columns.
  function analyze(res) {
    const T = res.types, cols = res.columns;
    const time = T.findIndex(isTime);
    const nums = T.map((t, i) => i).filter(i => i !== time && isNum(T[i])).slice(0, 5);
    const text = T.findIndex((t, i) => i !== time && isText(t));
    if (!nums.length || !res.rows.length || (time < 0 && text < 0)) return null;
    const xCol = time >= 0 ? time : text;
    const xs = [], xIndex = new Map();
    const xOf = v => {
      const k = v == null ? '∅' : String(v);
      if (!xIndex.has(k)) { xIndex.set(k, xs.length); xs.push(k); }
      return xIndex.get(k);
    };
    const num = v => v == null ? 0 : Number(v) || 0;
    let series, why, modes, mode;
    if (time >= 0 && text >= 0) {
      const totals = new Map();
      res.rows.forEach(r => { const g = r[text] == null ? '∅' : String(r[text]); totals.set(g, (totals.get(g) || 0) + Math.abs(num(r[nums[0]]))); });
      const ranked = [...totals.keys()].sort((a, b) => totals.get(b) - totals.get(a));
      const keep = ranked.slice(0, SERIES.length);
      const folded = ranked.length - keep.length;
      series = keep.map((g, k) => ({ name: g, c: SERIES[k], v: [] }));
      if (folded) series.push({ name: 'OTHER · ' + folded + ' FOLDED', c: OTHER, v: [] });
      res.rows.forEach(r => xOf(r[time]));
      xs.sort();
      xs.forEach((k, i) => xIndex.set(k, i));
      series.forEach(s => { s.v = xs.map(() => 0); });
      res.rows.forEach(r => {
        const g = r[text] == null ? '∅' : String(r[text]);
        const s = series[keep.indexOf(g)] || series[series.length - 1];
        s.v[xIndex.get(String(r[time]))] += num(r[nums[0]]);
      });
      why = 'AUTO: one date, one text, one number → stacked columns over time.' + (folded ? ' ' + folded + ' small ' + (folded === 1 ? 'category' : 'categories') + ' folded into OTHER.' : '');
      modes = [['stacked', 'Auto · stacked'], ['grouped', 'Grouped'], ['lines', 'Lines'], ['table', 'Table']];
      mode = 'stacked';
      return { xs, series, why, modes, mode, time: true, x: cols[time], color: cols[text], y: cols[nums[0]] };
    }
    res.rows.forEach(r => xOf(r[xCol]));
    if (time >= 0) { xs.sort(); xs.forEach((k, i) => xIndex.set(k, i)); }
    let xsKept = xs;
    series = nums.map((n, k) => ({ name: cols[n], c: SERIES[k], v: xs.map(() => 0) }));
    res.rows.forEach(r => { const i = xIndex.get(r[xCol] == null ? '∅' : String(r[xCol])); series.forEach((s, k) => { s.v[i] += num(r[nums[k]]); }); });
    if (time < 0 && xs.length > 24) {
      // Keep the 23 largest rows and fold the rest.
      const order = xs.map((_, i) => i).sort((a, b) => Math.abs(series[0].v[b]) - Math.abs(series[0].v[a]));
      const keep = new Set(order.slice(0, 23));
      xsKept = xs.filter((_, i) => keep.has(i)).concat(['OTHER · ' + (xs.length - 23)]);
      series.forEach(s => {
        const rest = s.v.filter((_, i) => !keep.has(i)).reduce((a, b) => a + b, 0);
        s.v = s.v.filter((_, i) => keep.has(i)).concat([rest]);
      });
    }
    if (time >= 0) {
      why = 'AUTO: dates and numbers → a line for each number column.';
      modes = [['lines', 'Auto · lines'], ['grouped', 'Columns'], ['table', 'Table']];
      mode = xs.length > 1 ? 'lines' : 'grouped';
    } else {
      why = 'AUTO: one text, numbers → a column for each row.' + (xsKept !== xs ? ' Smaller rows folded into OTHER.' : '');
      modes = [['grouped', 'Auto · columns'], ['table', 'Table']];
      mode = 'grouped';
    }
    return { xs: xsKept, series, why, modes, mode, time: time >= 0, x: cols[xCol], y: nums.map(n => cols[n]).join(', ') };
  }

  const xLabel = (k, time, multiYear, monthly) => {
    if (!time || k === '∅') return k.length > 14 ? k.slice(0, 13) + '…' : k;
    if (monthly) return fmtDate(k, { month: 'short' }).toUpperCase() + (multiYear && k.slice(5, 7) === '01' ? ' ' + k.slice(2, 4) : '');
    return shortDate(k);
  };

  function drawChart(el, a, mode, hover) {
    const n = a.xs.length, band = 100 / n;
    const monthly = a.time && a.xs.every(k => /^\d{4}-\d{2}-01/.test(k));
    const multiYear = a.time && a.xs[0].slice(0, 4) !== a.xs[n - 1].slice(0, 4);
    const thisMonth = cfg.today.slice(0, 8) + '01';
    const partial = monthly && a.xs[n - 1].slice(0, 10) === thisMonth;
    const label = i => xLabel(a.xs[i], a.time, multiYear, monthly) + (partial && i === n - 1 ? '*' : '');
    const totals = a.xs.map((_, i) => a.series.reduce((s, x) => s + x.v[i], 0));
    if (mode === 'table') {
      put(el, html`<div class="scroll" style="padding-top: 18px; padding-bottom: 20px"><div class="grid-table" style="min-width: ${Math.max(600, 140 * (a.series.length + 2))}px">
        <div class="rhead"><span class="col"><small>${a.x.toUpperCase()}</small></span>${a.series.map(s => html`<span class="col r"><small>${s.name}</small></span>`)}${a.series.length > 1 ? html`<span class="col r"><small>TOTAL</small></span>` : ''}</div>
        ${a.xs.map((k, i) => html`<div class="rrow"><span class="col">${a.time ? label(i) : k}</span>${a.series.map(s => html`<span class="col r${s.v[i] ? '' : ' null'}">${s.v[i] ? s.v[i].toLocaleString('en-US', { maximumFractionDigits: 2 }) : '—'}</span>`)}${a.series.length > 1 ? html`<span class="col r">${totals[i].toLocaleString('en-US', { maximumFractionDigits: 2 })}</span>` : ''}</div>`)}
      </div></div>`);
      return;
    }
    const stacked = mode === 'stacked';
    let lo = 0, hi = 0;
    if (stacked) {
      a.xs.forEach((_, i) => {
        let p = 0, m = 0;
        a.series.forEach(s => { if (s.v[i] > 0) p += s.v[i]; else m += s.v[i]; });
        hi = Math.max(hi, p); lo = Math.min(lo, m);
      });
    } else {
      a.series.forEach(s => s.v.forEach(v => { hi = Math.max(hi, v); lo = Math.min(lo, v); }));
    }
    const ax = axis(lo, hi, 5);
    const Yp = v => (v - ax.min) / (ax.max - ax.min) * 100; // percent from the bottom
    const zero = Yp(0);
    const rects = [], lines = [], marks = [], tops = [];
    if (stacked) {
      a.xs.forEach((_, i) => {
        let up = 0, down = 0;
        a.series.forEach(s => {
          const v = s.v[i];
          if (!v) return;
          const from = v > 0 ? up : down + v;
          if (v > 0) up += v; else down += v;
          rects.push({ center: (i + 0.5) * band, w: band * 0.62, b: Yp(from), h: Yp(from + Math.abs(v)) - Yp(from), c: s.c, gap: 2 });
        });
      });
      const iMax = totals.indexOf(Math.max(...totals));
      [...new Set([iMax, n - 1])].forEach(i => tops.push({ x: (i + 0.5) * band, b: Yp(totals[i] > 0 ? totals[i] : 0), label: money(totals[i]) }));
    } else if (mode === 'grouped') {
      const inner = band * (a.series.length > 1 ? 0.76 : 0.5);
      const bw = inner / a.series.length;
      a.xs.forEach((_, i) => a.series.forEach((s, j) => {
        const v = s.v[i];
        if (!v) return;
        const b = v > 0 ? zero : Yp(v);
        rects.push({ l: (i + 0.5) * band - inner / 2 + j * bw, w: bw, b, h: Math.abs(Yp(v) - zero), c: s.c });
      }));
    } else {
      a.series.forEach(s => {
        const pts = s.v.map((v, i) => [(i + 0.5) / n * 1000, 300 - Yp(v) * 3]);
        lines.push({ c: s.c, d: pts.map((p, i) => (i ? 'L' : 'M') + p[0].toFixed(1) + ',' + p[1].toFixed(1)).join('') });
        marks.push({ c: s.c, x: pts[n - 1][0] / 10, y: pts[n - 1][1] / 3 });
      });
    }
    const step = Math.ceil(n / 12);
    put(el, html`
      <div class="chart-legend legend">${a.series.map(s => html`<span><i class="key sq" style="background: ${s.c}"></i>${s.name}</span>`)}</div>
      <div class="plot-pad y-left" style="padding-top: 36px; padding-bottom: 16px">
        <div class="plot" style="height: 340px" id="q-plot">
          ${ax.ticks.map(v => html`<div class="hrule${v === ax.min ? ' base' : ''}" style="top: ${(100 - Yp(v)).toFixed(2)}%"></div><div class="ylab out" style="top: ${(100 - Yp(v)).toFixed(2)}%">${compact(v)}</div>`)}
          <div id="q-band"></div>
          ${rects.map(r => r.center != null
            ? html`<div class="bar-rect" style="left: calc(${r.center.toFixed(3)}% - min(12px, ${(r.w / 2).toFixed(3)}%)); width: min(24px, ${r.w.toFixed(3)}%); bottom: ${r.b.toFixed(3)}%; height: max(1px, calc(${r.h.toFixed(3)}% - ${r.gap}px)); background: ${r.c}"></div>`
            : html`<div class="bar-rect" style="left: calc(${r.l.toFixed(3)}% + 1px); width: max(1px, calc(${r.w.toFixed(3)}% - 2px)); bottom: ${r.b.toFixed(3)}%; height: ${r.h.toFixed(3)}%; background: ${r.c}"></div>`)}
          ${lines.length ? html`<svg class="lines" viewBox="0 0 1000 300" preserveAspectRatio="none" aria-hidden="true">${lines.map(l => html`<path d="${l.d}" fill="none" stroke="${l.c}" stroke-width="2" stroke-linejoin="miter"></path>`)}</svg>` : ''}
          ${marks.map(k => html`<div class="dot" style="left: ${k.x.toFixed(2)}%; top: ${k.y.toFixed(2)}%; width: 8px; height: 8px; margin: -4px 0 0 -4px; background: ${k.c}; box-shadow: 0 0 0 2px #0B1118"></div>`)}
          ${tops.map(t => html`<div class="top-lab" style="left: ${t.x.toFixed(3)}%; bottom: calc(${t.b.toFixed(3)}% + 8px)">${t.label}</div>`)}
          ${a.xs.map((_, i) => html`<div class="band" data-i="${i}" style="left: ${(i * band).toFixed(3)}%; width: ${band.toFixed(3)}%"></div>`)}
          <div id="q-tip"></div>
        </div>
        ${xAxis(a.xs.map((_, i) => i).filter(i => i % step === 0).map(i => ({ left: ((i + 0.5) * band).toFixed(3), label: label(i), tx: '-50%' })))}
        ${partial ? html`<div class="foot-note">* ${monthName(thisMonth)} is month to date</div>` : ''}
      </div>`);
    const tip = i => {
      if (i == null) { put($('#q-band'), ''); put($('#q-tip'), ''); return; }
      const x = (i + 0.5) * band, right = x > 62;
      put($('#q-band'), html`<div class="band-hi" style="left: ${(i * band).toFixed(3)}%; width: ${band.toFixed(3)}%"></div>`);
      put($('#q-tip'), html`<div class="tip" style="top: 0; left: ${x.toFixed(3)}%; min-width: 240px; transform: translateX(${right ? '-100%' : '0%'}); margin-left: ${right ? '-28px' : '28px'}">
        <div class="t">${a.time ? (monthly ? fmtDate(a.xs[i], { month: 'long', year: 'numeric' }) : longDate(a.xs[i])) + (partial && i === n - 1 ? ' · to date' : '') : a.xs[i]}</div>
        ${a.series.slice().reverse().map(s => html`<div class="r"><span><i class="sw" style="background: ${s.c}"></i>${s.name}</span><span>${s.v[i].toLocaleString('en-US', { maximumFractionDigits: 2 })}</span></div>`)}
        ${a.series.length > 1 ? html`<div class="r total"><span>TOTAL</span><span>${totals[i].toLocaleString('en-US', { maximumFractionDigits: 2 })}</span></div>` : ''}
      </div>`);
    };
    const plot = $('#q-plot');
    let last = null;
    plot.addEventListener('mouseover', e => {
      const b = e.target.closest('.band');
      if (b && +b.dataset.i !== last) { last = +b.dataset.i; tip(last); }
    });
    plot.addEventListener('mouseleave', () => { last = null; tip(null); });
    if (hover != null) tip(hover);
  }

  async function query() {
    const params = new URLSearchParams(location.search);
    let sql = params.get('q') || DEFAULT_SQL;
    let mode = null, analysis = null, result = null, shown = 100;

    put(main, html`
      <section class="panel hud q-panel" aria-label="Query">
        ${head('01', 'Query · ' + cfg.db, html`<span>Read-only connection</span>`)}
        <div class="editor">
          <div class="gutter" aria-hidden="true" id="gutter"></div>
          <div class="code"><pre class="hl" aria-hidden="true" id="hl"></pre><textarea id="sql" spellcheck="false" autocapitalize="off" autocomplete="off" aria-label="SQL query"></textarea></div>
        </div>
        <div class="q-bar">
          <div class="q-stat label" id="q-stat"></div>
          <div class="q-actions">
            <button type="button" class="btn" id="copy-sql">Copy as fin sql</button>
            <button type="button" class="btn run" id="run">Run<span aria-hidden="true">⌘↵</span></button>
          </div>
        </div>
        <div id="q-err"></div>
      </section>
      <section class="panel hud mt" aria-label="Chart" id="chart-panel">
        ${head('02', 'Chart', html`<div class="pills" id="pills"></div>`)}
        <div id="chart"></div>
      </section>
      <section class="panel hud mt" aria-label="Result">
        ${head('03', 'Result', html`<span id="dims"></span>`)}
        <div id="result"></div>
      </section>`);

    const ta = $('#sql');
    const paint = () => {
      $('#hl').innerHTML = highlight(ta.value) + '\n';
      $('#gutter').textContent = ta.value.split('\n').map((_, i) => i + 1).join('\n');
      const one = ta.value.replace(/\s+/g, ' ').trim();
      setCommand(html`sql "${one.length > 44 ? one.slice(0, 43) + '…' : one}"`, 'sql ' + shq(ta.value.trim()));
    };
    ta.value = sql;
    paint();
    ta.addEventListener('input', paint);
    ta.addEventListener('keydown', e => {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); run(); }
      if (e.key === 'Tab' && !e.shiftKey) { e.preventDefault(); ta.setRangeText('  ', ta.selectionStart, ta.selectionEnd, 'end'); paint(); }
    });
    $('#run').addEventListener('click', () => run());
    $('#copy-sql').addEventListener('click', e => copy('fin sql ' + shq(ta.value.trim()), e.currentTarget));

    const renderChart = () => {
      const el = $('#chart');
      if (!analysis) {
        put($('#pills'), '');
        put(el, empty('Nothing to chart', 'A chart needs a number column, and a date or text column to group it by. The rows are below.'));
        return;
      }
      put($('#pills'), html`<span>X <b>${analysis.x}</b></span>${analysis.color ? html`<span>Color <b>${analysis.color}</b></span>` : ''}<span>Y <b>${analysis.y}</b></span>`);
      const WHY = { grouped: 'Columns: compare the values side by side.', lines: 'Lines: each series’ trend on its own path.', table: 'Table: the result pivoted, one row per ' + analysis.x + '.' };
      const why = mode === analysis.mode ? analysis.why : WHY[mode] || analysis.why;
      put(el, html`
        <div class="chart-tools">
          <div class="seg well" role="group" aria-label="Chart type" id="modes">${analysis.modes.map(([m, name]) => html`<button type="button" data-mode="${m}" aria-pressed="${m === mode}">${name}</button>`)}</div>
          <div class="why">${why}</div>
        </div>
        <div id="plot-area"></div>`);
      drawChart($('#plot-area'), analysis, mode);
    };
    $('#chart-panel').addEventListener('click', e => {
      const b = e.target.closest('[data-mode]');
      if (b && b.dataset.mode !== mode) { mode = b.dataset.mode; renderChart(); }
    });

    const cell = (v, t) => {
      if (v == null) return html`<span class="null">NULL</span>`;
      if (typeof v === 'object') return JSON.stringify(v);
      if (isNum(t) && typeof v === 'number') return v.toLocaleString('en-US', { maximumFractionDigits: 6, useGrouping: false });
      if (isTime(t) && typeof v === 'string' && /T00:00:00(\.0+)?Z$/.test(v)) return v.slice(0, 10);
      return String(v);
    };
    const rowsLabel = r => r.row_count.toLocaleString('en-US') + (r.truncated ? '+ rows' : r.row_count === 1 ? ' row' : ' rows');
    const renderResult = () => {
      const r = result;
      put($('#dims'), r ? rowsLabel(r) + ' × ' + r.columns.length + (r.columns.length === 1 ? ' column' : ' columns') : '');
      if (!r) { put($('#result'), ''); return; }
      const rows = r.rows.slice(0, shown);
      put($('#result'), html`
        <div class="scroll"><div class="grid-table" style="min-width: ${Math.max(620, 40 + 156 * r.columns.length)}px">
          <div class="rhead"><span class="idx"><small>#</small></span>${r.columns.map((c, i) => html`<span class="col${isNum(r.types[i]) ? ' r' : ''}"><b>${c}</b><small>${r.types[i]}</small></span>`)}</div>
          ${rows.map((row, k) => html`<div class="rrow"><span class="idx">${k + 1}</span>${row.map((v, i) => html`<span class="col${isNum(r.types[i]) ? ' r' : ''}" title="${v == null ? '' : typeof v === 'object' ? JSON.stringify(v) : String(v)}">${cell(v, r.types[i])}</span>`)}</div>`)}
        </div></div>
        <div class="panel-foot">
          <span class="caps">Showing ${rows.length} of ${r.truncated ? 'the first ' + r.rows.length.toLocaleString('en-US') + '; fin sql has the rest' : r.row_count}</span>
          ${rows.length < r.rows.length ? html`<button type="button" class="btn" id="more-rows">Show ${Math.min(500, r.rows.length - rows.length)} more</button>` : ''}
          <span class="caps">Same rows as fin sql … --json</span>
        </div>`);
    };
    $('#result').addEventListener('click', e => {
      if (e.target.closest('#more-rows')) { shown += 500; renderResult(); }
    });

    async function run() {
      const text = ta.value.trim();
      if (!text) return;
      const btn = $('#run');
      btn.disabled = true;
      put($('#q-stat'), html`<span class="ok"><i></i>Running</span>`);
      try {
        const r = await api('./api/query', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ sql: text }) });
        result = r;
        shown = 100;
        analysis = analyze(r);
        mode = analysis ? analysis.mode : null;
        put($('#q-err'), '');
        put($('#q-stat'), html`<span class="ok"><i></i>OK</span><span>${rowsLabel(r)}</span><span>${r.elapsed_ms} ms</span><span>${r.columns.length} ${r.columns.length === 1 ? 'column' : 'columns'}</span>`);
        const u = new URL(location.href);
        if (text === DEFAULT_SQL) u.searchParams.delete('q'); else u.searchParams.set('q', text);
        history.replaceState(null, '', u);
      } catch (err) {
        result = null; analysis = null;
        put($('#q-stat'), html`<span class="bad"><i></i>${err.code === 'SQL_ERROR' ? 'SQL error' : 'Error'}</span>`);
        put($('#q-err'), html`<div class="q-err">${err.message}</div>`);
      } finally {
        btn.disabled = false;
      }
      renderChart();
      renderResult();
    }
    run();
  }

  // --- start ---

  const pages = { overview, spending, portfolio, query };
  (pages[cfg.page] || overview)().catch(err => {
    put(main, html`<div class="error">${err.message}${err.code ? '  ' + err.code : ''}</div>`);
  });
})();
