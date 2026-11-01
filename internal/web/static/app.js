'use strict';

// Hostnames and option values originate from DHCP clients. All text therefore
// goes through textContent; this file must never assign innerHTML.
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else if (k === 'value') el.value = v;
    else if (k === 'checked' || k === 'disabled' || k === 'required' || k === 'selected') el[k] = true;
    else el.setAttribute(k, v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c);
  }
  return el;
}

const view = document.getElementById('view');
const dialog = document.getElementById('dialog');
let dialogGeneration = 0;
let meta = { writable: false, serverName: '' };

async function api(method, path, body) {
  const res = await fetch('api/' + path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { /* non-JSON error page */ }
  if (data && data.status && data.steps) return data; // change result, also on failure
  if (!res.ok) {
    const err = new Error((data && data.error) || `HTTP ${res.status}`);
    err.problems = data && data.problems;
    throw err;
  }
  return data;
}

function ipKey(ip) {
  return (ip || '').split('.').reduce((n, part) => n * 256 + Number(part), 0);
}

const DATE_LOCALE = 'de-DE';

function fmtDuration(sec) {
  if (sec === null || sec === undefined) return '–';
  if (sec <= 0) return '0s';
  const d = Math.floor(sec / 86400), hr = Math.floor(sec / 3600) % 24;
  const m = Math.floor(sec / 60) % 60, s = sec % 60;
  if (d) return `${d}d ${hr}h`;
  if (hr) return `${hr}h ${m}m`;
  if (m) return `${m}m ${s}s`;
  return `${s}s`;
}

function fmtTime(t) {
  return t ? new Date(t).toLocaleString(DATE_LOCALE).replace(', ', ' ') : '–';
}

function fmtShort(t) {
  return new Date(t).toLocaleString(DATE_LOCALE, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }).replace(', ', ' ');
}

function fmtCount(n) {
  return n === null || n === undefined ? '–' : n.toLocaleString(undefined);
}

function state(kind, text) {
  return h('span', { class: 'state ' + kind }, text);
}

function notes(list) {
  if (!list || !list.length) return null;
  return h('div', { class: 'notes' }, list.map(w => h('div', { class: 'note ' + w.severity },
    (w.severity === 'crit' ? 'Error: ' : 'Warning: ') + w.message)));
}

function errorNote(err) {
  return h('div', { class: 'notes' }, h('div', { class: 'note crit' }, 'Error: ' + err.message));
}

// Table with search, filters and sorting. Data stays in the browser, so typing
// does not trigger a lease4-get-all on every key press.
function dataTable({ columns, search, filters = [], empty, initialSort, actions = [], params = new URLSearchParams(), rowClass, plain }) {
  const sort = { key: initialSort, dir: 1 };
  const query = { text: (params.get('q') || '').toLowerCase() };
  const filterValues = Object.fromEntries(filters.map(f => [f.key, params.get(f.key) ?? f.default ?? '']));
  let rows = [];

  const count = h('span', { class: 'count' });
  const head = h('thead');
  const body = h('tbody');
  const filterEls = filters.map(f => {
    const select = h('select', { 'aria-label': f.label, onchange: () => { filterValues[f.key] = select.value; draw(); } });
    return { def: f, select };
  });
  const input = h('input', {
    class: 'grow', type: 'search', placeholder: search, 'aria-label': search, value: params.get('q') || '',
    oninput: () => { query.text = input.value.trim().toLowerCase(); draw(); },
  });

  function sortValue(col, row) {
    return col.sort ? col.sort(row) : (row[col.key] ?? '');
  }

  function draw() {
    let list = rows.filter(row => {
      if (query.text && !columns.some(c => c.search && String(c.search(row) ?? '').toLowerCase().includes(query.text))) return false;
      return filters.every(f => !filterValues[f.key] || f.match(row, filterValues[f.key]));
    });
    const col = columns.find(c => c.key === sort.key);
    if (col) {
      list = list.toSorted((a, b) => {
        const x = sortValue(col, a), y = sortValue(col, b);
        const cmp = typeof x === 'number' && typeof y === 'number' ? x - y : String(x).localeCompare(String(y), undefined, { numeric: true });
        return cmp * sort.dir;
      });
    }
    head.replaceChildren(h('tr', {}, columns.map(c => h('th', {
      class: c.noSort ? '' : 'sortable',
      'aria-sort': c.key === sort.key ? (sort.dir > 0 ? 'ascending' : 'descending') : undefined,
      onclick: c.noSort ? undefined : () => {
        sort.dir = sort.key === c.key ? -sort.dir : 1;
        sort.key = c.key;
        draw();
      },
    }, c.label + (c.key === sort.key ? (sort.dir > 0 ? ' ▲' : ' ▼') : ''))), actions.length ? h('th', {}) : null));
    const cells = list.map(row => h('tr', { class: rowClass?.(row) },
      columns.map(c => h('td', { class: c.class || '' }, c.render ? c.render(row) : (row[c.key] || '–'))),
      actions.length ? h('td', { class: 'actions' }, actions.map(a => a(row))) : null));
    body.replaceChildren(...(cells.length ? cells
      : [h('tr', {}, h('td', { colspan: columns.length + 1, class: 'empty' }, rows.length ? 'No matches' : empty))]));
    count.textContent = `${list.length} / ${rows.length}`;
  }

  return {
    el: h('div', {},
      plain ? null : h('div', { class: 'toolbar' }, input, filterEls.map(f => f.select), count),
      h('div', { class: 'panel table-wrap' }, h('table', {}, head, body))),
    setRows(next) {
      rows = next;
      for (const { def, select } of filterEls) {
        const current = filterValues[def.key];
        select.replaceChildren(h('option', { value: '' }, def.label), ...def.options(rows).map(o => h('option', { value: o.value }, o.label)));
        select.value = def.options(rows).some(o => o.value === current) ? current : '';
        filterValues[def.key] = select.value;
      }
      draw();
    },
  };
}

// Periodic reload of a page; the interval is shared between pages.
let timer = null;
function stopRefresh() { clearInterval(timer); timer = null; }

function refreshControls(load) {
  const saved = localStorage.getItem('refresh') || '30';
  const schedule = sec => {
    stopRefresh();
    if (Number(sec) > 0) {
      timer = setInterval(() => { if (!dialog.open && !document.hidden) load(); }, sec * 1000);
    }
  };
  const select = h('select', {
    'aria-label': 'Refresh automatically',
    onchange: () => { localStorage.setItem('refresh', select.value); schedule(select.value); },
  }, [['0', 'Auto: off'], ['10', 'Auto: 10 s'], ['30', 'Auto: 30 s'], ['60', 'Auto: 60 s'], ['300', 'Auto: 5 min']]
    .map(([value, label]) => h('option', { value }, label)));
  select.value = saved;
  schedule(saved);
  return [select, h('button', { onclick: load }, 'Refresh')];
}

// The client popup closes on a click next to it. Only one that also started there
// counts, so that selecting text inside can end anywhere.
function outsideDialog(event) {
  const r = dialog.getBoundingClientRect();
  return event.clientX < r.left || event.clientX > r.right || event.clientY < r.top || event.clientY > r.bottom;
}
let pressedOutside = false;
dialog.addEventListener('mousedown', event => { pressedOutside = outsideDialog(event); });
dialog.addEventListener('click', event => {
  if (dialog.classList.contains('wide') && pressedOutside && outsideDialog(event)) closeDialog();
});

function closeDialog() { dialogGeneration++; dialog.close(); dialog.replaceChildren(); dialog.classList.remove('wide'); }

function showDialog(...content) {
  dialogGeneration++;
  dialog.classList.remove('wide');
  dialog.replaceChildren(...content.filter(Boolean));
  if (!dialog.open) dialog.showModal();
}

function confirmDialog(title, text, label, action) {
  const error = h('div');
  const button = h('button', {
    class: 'danger solid',
    onclick: async () => {
      button.disabled = true;
      try {
        await action();
      } catch (err) {
        error.replaceChildren(errorNote(err));
        button.disabled = false;
      }
    },
  }, label);
  showDialog(h('h2', {}, title), h('p', {}, text), error,
    h('div', { class: 'dialog-actions' }, h('button', { onclick: closeDialog }, 'Cancel'), button));
}

// Shows the outcome of a change. Anything but "ok" keeps the dialog open, lists
// each step and offers the only follow-up that is safe: repeating config-write.
function showResult(res, reload) {
  const title = { ok: 'Change succeeded', failed: 'Change failed', partial: 'Change only partially applied' }[res.status];
  const kind = { ok: 'ok', failed: 'crit', partial: 'crit' }[res.status];
  const persistFailed = res.steps.some(s => s.name.includes('config-write') && !s.ok);
  const stepList = h('ul', { class: 'steps' }, res.steps.map(s => h('li', {},
    state(s.ok ? 'ok' : 'crit', s.name),
    s.message ? h('span', { class: 'detail' }, s.message) : null)));
  const warnings = res.warnings.map(w => h('div', { class: 'note warn' }, 'Note: ' + w));
  showDialog(
    h('h2', {}, title),
    h('div', { class: 'notes' }, h('div', { class: 'note ' + kind }, res.message), warnings),
    stepList,
    res.backup ? h('p', { class: 'muted' }, 'Backup: ' + res.backup) : null,
    h('div', { class: 'dialog-actions' },
      persistFailed ? h('button', {
        class: 'primary',
        onclick: async () => {
          try {
            showResult(await api('POST', 'config/write'), reload);
          } catch (err) {
            dialog.append(errorNote(err));
          }
        },
      }, 'Save again') : null,
      h('button', { onclick: () => { closeDialog(); reload(); } }, 'Close')));
}

const pages = {};

pages.dashboard = () => {
  const body = h('div', {});
  const load = async () => {
    let d;
    try {
      d = await api('GET', 'dashboard');
    } catch (err) {
      body.replaceChildren(errorNote(err));
      return;
    }
    updateChip(d.server);
    const ha = d.ha[0];
    body.replaceChildren(...[
      notes(d.warnings),
      h('div', { class: 'tiles' },
        tile('Server', d.server.reachable ? state('ok', 'reachable') : state('crit', 'unreachable'), d.server.name),
        tile('Kea version', d.server.version || '–', d.server.uptime ? 'Uptime ' + fmtDuration(d.server.uptime) : ''),
        tile('HA state', ha ? state(d.ha[0].severity, ha['ha-servers'].local.state) : state('neutral', d.server.reachable ? 'no HA' : 'unknown'),
          ha ? ha['ha-servers'].local.role : '', '#/ha'),
        tile('Active leases', fmtCount(d.activeLeases), d.totalLeases != null ? `${fmtCount(d.totalLeases)} total` : '', '#/leases'),
        tile('Reservations', fmtCount(d.reservations), '', '#/reservations'),
        tile('Subnets', fmtCount(d.subnetCount), '', '#/subnets'),
        tile('Last contact', d.server.lastContact ? h('span', { title: d.server.url }, new Date(d.server.lastContact).toLocaleTimeString(DATE_LOCALE)) : '–')),
      d.servers.length ? h('h2', {}, 'Servers') : null,
      d.servers.length ? serverOverview(d.servers) : null,
      d.recentLeases.length ? h('h2', {}, 'Recent leases') : null,
      d.recentLeases.length ? recentLeases(d.recentLeases, load) : null,
      h('h2', {}, 'Pool utilization'),
      h('div', { class: 'panel pad' }, d.subnets.length
        ? d.subnets.map(s => meterRow(s.subnet, s.assigned, s.total, s.percent, '#/subnets'))
        : h('div', { class: 'empty' }, d.server.reachable ? 'No subnets' : 'No data')),
    ].filter(Boolean));
  };
  view.replaceChildren(h('div', { class: 'page-head' }, h('h1', {}, 'Dashboard'), refreshControls(load)), body);
  load();
};

function recentLeases(leases, reload) {
  const notice = h('div');
  // Last contact shows why a lease is listed.
  const columns = leaseColumns();
  columns.splice(5, 0, {
    key: 'started', label: 'Last contact', class: 'mono', sort: r => -new Date(r.started).getTime(),
    render: r => h('span', { title: fmtTime(r.started) }, fmtAge(r.started)),
  });
  const table = dataTable({ columns, plain: true, empty: 'No leases', actions: leaseActions(reload, notice) });
  table.setRows(leases);
  return h('div', {}, notice, table.el);
}

function tile(label, value, sub, href) {
  return h(href ? 'a' : 'div', { class: 'tile', href }, h('div', { class: 'label' }, label), h('div', { class: 'value' }, value),
    sub ? h('div', { class: 'sub' }, sub) : null);
}

function meterRow(name, assigned, total, percent, href) {
  const fill = h('div', {});
  fill.style.width = Math.min(percent ?? 0, 100) + '%';
  const title = percent == null ? 'no statistics' : `${assigned} of ${total} addresses assigned`;
  return h('div', { class: 'meter-row', title },
    h('div', { class: 'name' }, href ? h('a', { href }, name) : name),
    h('div', { class: 'meter' }, fill),
    h('div', { class: 'num' }, percent == null ? 'no statistics' : `${fmtCount(assigned)} / ${fmtCount(total)} · ${percent.toFixed(0)} %`));
}

const leaseStateKind = { active: 'ok', expired: 'neutral', released: 'neutral', declined: 'warn' };

function leaseColumns() {
  return [
      { key: 'ipAddress', label: 'IP', class: 'mono', sort: r => ipKey(r.ipAddress), search: r => r.ipAddress, render: r => clientLink(r.ipAddress) },
      { key: 'hwAddress', label: 'MAC', class: 'mono', search: r => `${r.hwAddress} ${r.vendor || ''}`, render: r => macCell(r.hwAddress, r.vendor) },
      { key: 'clientId', label: 'Client-ID', class: 'mono clip', search: r => r.clientId, render: r => r.clientId ? h('span', { title: r.clientId }, r.clientId) : '–' },
      {
        key: 'hostname', label: 'Hostname', search: r => r.hostname,
        render: r => [r.hostname || '–', r.new ? ' ' : null, r.new ? h('span', { class: 'tag', title: 'First seen on the network within the last day' }, 'new') : null],
      },
      {
        key: 'reservation', label: 'Reservation', class: 'mono wrap', sort: r => r.reservation?.ipAddress ? ipKey(r.reservation.ipAddress) : -1,
        search: r => `${r.reservation?.hostname ?? ''} ${(r.reservation?.classes ?? []).join(' ')}`,
        render: reservationCell,
      },
      { key: 'wifi', label: 'AP', class: 'mono', search: r => `${r.wifi?.accessPoint ?? ''} ${r.wifi?.interface ?? ''}`, sort: r => r.wifi?.accessPoint ?? '', render: r => whereCell(r.wifi, false) },
      { key: 'subnetId', label: 'Subnet', class: 'mono', render: r => String(r.subnetId) },
      { key: 'state', label: 'Status', render: r => state(leaseStateKind[r.state] || 'warn', r.state) },
      { key: 'expires', label: 'Expires', class: 'mono', render: r => h('span', { title: fmtTime(r.expires) }, fmtShort(r.expires)) },
      { key: 'remaining', label: 'Remaining', class: 'mono', render: r => fmtDuration(r.remaining) },
  ];
}

function leaseActions(reload, notice) {
  return meta.writable ? [r => r.reservation || !(r.hwAddress || r.clientId) ? null : h('button', {
      class: 'small',
      title: 'Create a reservation from this lease',
      onclick: async () => {
        try {
          await reserveLease(r, reload);
        } catch (err) {
          notice.replaceChildren(errorNote(err));
        }
      },
    }, 'Reserve'), r => h('button', {
      class: 'small danger',
      onclick: () => confirmDialog('Delete lease', `Really delete lease ${r.ipAddress} (${r.hostname || r.hwAddress})?`, 'Delete', async () => {
        await api('DELETE', 'leases/' + encodeURIComponent(r.ipAddress));
        closeDialog();
        reload();
      }),
    }, 'Delete')] : [];
}

pages.leases = params => {
  const notice = h('div');
  const table = dataTable({
    params,
    search: 'Search IP, MAC, vendor or hostname',
    empty: 'No leases',
    initialSort: 'ipAddress',
    filters: [
      {
        key: 'subnet', label: 'All subnets',
        options: rows => [...new Map(rows.map(r => [r.subnetId, r.subnet || `Subnet ${r.subnetId}`]))].sort(([a], [b]) => a - b).map(([id, label]) => ({ value: String(id), label })),
        match: (r, v) => String(r.subnetId) === v,
      },
      {
        key: 'state', label: 'All states',
        options: () => [{ value: 'active', label: 'Active only' }, { value: 'inactive', label: 'Expired only' }],
        match: (r, v) => r.active === (v === 'active'),
      },
    ],
    columns: leaseColumns(),
    actions: leaseActions(() => load(), notice),
  });
  const load = async () => {
    try {
      const list = await api('GET', 'leases');
      table.setRows(list.leases);
      notice.replaceChildren(...[notes(list.warnings)].filter(Boolean));
    } catch (err) {
      notice.replaceChildren(errorNote(err));
    }
  };
  view.replaceChildren(h('div', { class: 'page-head' }, h('h1', {}, 'Leases'), refreshControls(load)), notice, table.el);
  load();
};

// Opens the create dialog prefilled from a lease. Everything stays editable, so
// the client can be given a different address than the one it holds now.
async function reserveLease(lease, reload) {
  const [net, list] = await Promise.all([api('GET', 'subnets'), api('GET', 'reservations')]);
  const defaultGlobal = list.reservations.length > 0 && list.reservations.every(r => r.global);
  reservationDialog(net.subnets, null, reload, defaultGlobal, {
    identifierType: lease.hwAddress ? 'hw-address' : 'client-id',
    identifier: lease.hwAddress || lease.clientId,
    ipAddress: lease.ipAddress,
    hostname: lease.hostname.trim().replace(/[^a-zA-Z0-9_.-]+/g, '-').replace(/^-+|-+$/g, ''),
  });
}

function searchLink(page, text) {
  return `#/${page}?q=${encodeURIComponent(text)}`;
}

// Opens the client popup from any table.
function clientLink(id, content) {
  return h('a', {
    href: '#',
    class: 'client-link',
    title: 'Show everything about this client',
    onclick: event => { event.preventDefault(); clientDialog(id); },
  }, content ?? id);
}

// MAC address with its vendor as tooltip.
function macCell(mac, vendor) {
  if (!mac) return '–';
  const link = text => /^[0-9a-f]{2}([:-][0-9a-f]{2}){5}$/i.test(mac) ? clientLink(mac, text) : text;
  if (vendor?.startsWith('Private MAC')) {
    return [link(mac), ' ', h('span', { class: 'state neutral', title: vendor }, 'local')];
  }
  return link(vendor ? h('span', { class: 'vendor', title: vendor }, mac) : mac);
}

// Reservation column of leases and devices.
function reservationCell(row) {
  const r = row.reservation;
  if (!r) return '–';
  if (r.conflict) {
    return [h('span', { class: 'state crit', title: r.conflict }, 'MAC mismatch'), ' ',
      h('a', { href: searchLink('reservations', r.identifier), title: r.conflict }, r.hostname || r.identifier)];
  }
  const label = r.ipAddress === row.ipAddress ? 'reserved' : `reserved: ${r.ipAddress || r.identifier}`;
  return [h('a', { href: searchLink('reservations', r.identifier) }, label), classTags(r.classes)];
}

// Where a client is connected. full adds the signal to the cell.
function whereCell(w, full) {
  if (!w) return '–';
  const title = `${w.accessPoint}, ${w.interface}, ${w.signal} dBm, connected for ${fmtDuration(w.connectedTime)}, last packet ${fmtDuration(Math.floor(w.inactive / 1000))} ago`;
  const strength = [-80, -70, -60, -50].filter(level => w.signal >= level).length;
  const bars = h('span', { class: 'bars', 'aria-hidden': 'true' },
    [1, 2, 3, 4].map(bar => h('i', { class: bar <= strength ? 'on' : '' })));
  return h('span', { class: 'where', title }, bars, w.accessPoint, full ? ` ${w.signal}` : '');
}

// Options of each client class by name, for the tooltip of its tag.
let classOptions = new Map();

async function loadClassOptions() {
  try {
    const list = await api('GET', 'classes');
    classOptions = new Map(list.classes.map(c => [c.name, c.options.map(o => `${o.name || o.code}=${o.data}`).join('\n')]));
  } catch {
    // without the details a tag still shows the name
  }
}

function classTags(classes) {
  return (classes ?? []).flatMap(name => [' ', h('a', { class: 'tag', href: '#/classes', title: classOptions.get(name) }, name)]);
}

function reservationPath(r) {
  return ['reservations', r.subnetId, r.identifierType, r.identifier].map(encodeURIComponent).join('/');
}

function field(label, control) {
  return h('div', { class: 'field' }, h('label', {}, label, control));
}

function optionRow(list, opt) {
  const name = h('input', { class: 'mono-input', placeholder: 'Name, e.g. ntp-servers', 'aria-label': 'Option name', value: opt.name || '', disabled: !opt.name && !!opt.code });
  const data = h('input', { class: 'mono-input', placeholder: 'Value', 'aria-label': 'Option value', value: opt.data || '' });
  const row = h('div', { class: 'option-row' }, name, data, h('button', { type: 'button', class: 'small', onclick: () => row.remove() }, '×'));
  row.read = () => ({ name: name.value.trim(), code: opt.code || 0, data: data.value });
  list.append(row);
}

async function reservationDialog(subnets, existing, reload, defaultGlobal, prefill) {
  const available = (await api('GET', 'classes').catch(() => ({ classes: [] }))).classes;
  const editing = !!existing;
  const r = existing || { identifierType: 'hw-address', options: [], ...prefill };
  const subnet = h('select', { disabled: editing, 'aria-label': 'Subnet' },
    subnets.map(s => h('option', { value: s.id }, `${s.id} – ${s.subnet}`)), h('option', { value: 0 }, '0 – global'));
  subnet.value = r.subnetId ?? (defaultGlobal ? 0 : subnets[0]?.id ?? 0);
  const idType = h('select', {},
    ['hw-address', 'client-id', 'duid', 'circuit-id', 'flex-id'].map(t => h('option', { value: t }, t)));
  idType.value = r.identifierType;
  const identifier = h('input', { class: 'mono-input', required: true, value: r.identifier || '', placeholder: 'aa:bb:cc:dd:ee:ff' });
  const ip = h('input', { class: 'mono-input', value: r.ipAddress || '', placeholder: '192.0.2.10' });
  const hostname = h('input', { class: 'mono-input', value: r.hostname || '' });
  const comment = h('input', { value: r.comment || '' });
  const classes = available.map(c => h('input', { type: 'checkbox', 'aria-label': c.name, checked: (r.classes || []).includes(c.name), 'data-name': c.name }));
  const options = h('div', {});
  (r.options || []).forEach(o => optionRow(options, o));
  const error = h('div');
  const save = h('button', { class: 'primary', type: 'submit' }, editing ? 'Save' : 'Create');

  const collect = () => ({
    subnetId: Number(subnet.value),
    identifierType: idType.value,
    identifier: identifier.value.trim(),
    ipAddress: ip.value.trim(),
    hostname: hostname.value.trim(),
    comment: comment.value.trim(),
    classes: classes.filter(c => c.checked).map(c => c.dataset.name),
    options: [...options.children].map(row => row.read()),
  });
  const body = () => {
    const input = collect();
    return editing ? { ...input, newIdentifierType: input.identifierType, newIdentifier: input.identifier } : input;
  };
  const checkPath = editing ? reservationPath(r) + '/check' : 'reservations/check';
  // Shows what is known about the address while it is entered.
  const hints = h('div', { class: 'notes' });
  let hintTimer, hintRun = 0;
  const showHints = async () => {
    const run = ++hintRun;
    const address = ip.value.trim();
    let notices = [];
    if (address) {
      try {
        const check = await api('POST', checkPath, body());
        notices = [...check.warnings, ...check.hints].map(text => ({ text, kind: 'warn' }));
      } catch (err) {
        // a conflict of this address is the news, not the rest of a half filled form
        notices = (err.problems || []).filter(p => p.includes(address) && !p.startsWith('invalid')).map(text => ({ text, kind: 'crit' }));
      }
    }
    if (run === hintRun) hints.replaceChildren(...notices.map(n => h('div', { class: `note ${n.kind} hint` }, n.text)));
  };
  ip.addEventListener('input', () => {
    clearTimeout(hintTimer);
    hintTimer = setTimeout(showHints, 300);
  });
  const form = h('form', {
    onsubmit: async ev => {
      ev.preventDefault();
      save.disabled = true;
      error.replaceChildren();
      const payload = body();
      const apply = async () => showResult(editing
        ? await api('PUT', reservationPath(r), payload)
        : await api('POST', 'reservations', payload), reload);
      try {
        const check = await api('POST', checkPath, payload);
        if (check.warnings.length) {
          // nothing is applied before the operator has seen the consequences
          showDialog(h('h2', {}, 'Apply this change?'),
            h('div', { class: 'notes' }, check.warnings.map(w => h('div', { class: 'note warn' }, w))),
            h('div', { class: 'dialog-actions' },
              h('button', { onclick: closeDialog }, 'Cancel'),
              h('button', { class: 'danger solid', onclick: () => apply().catch(err => dialog.append(errorNote(err))) }, 'Apply anyway')));
          return;
        }
        await apply();
      } catch (err) {
        const list = err.problems || [err.message];
        error.replaceChildren(h('div', { class: 'notes' }, list.map(p => h('div', { class: 'note crit' }, p))));
        save.disabled = false;
      }
    },
  },
    h('h2', {}, editing ? 'Edit reservation' : 'Create reservation'),
    h('div', { class: 'field-row' }, field('Subnet', subnet), field('Identifier type', idType)),
    field('Identifier', identifier),
    h('div', { class: 'field-row' }, field('IP address', ip), field('Hostname', hostname)),
    hints,
    field('Comment', comment),
    classes.length ? h('div', { class: 'field' }, h('label', {}, 'Client classes'),
      h('div', { class: 'class-list' }, classes.map((c, i) => h('label', {}, c, available[i].name)))) : null,
    h('div', { class: 'field' }, h('label', {}, 'DHCP options'), options,
      h('div', {}, h('button', { type: 'button', class: 'small', onclick: () => optionRow(options, {}) }, 'Add option'))),
    error,
    h('div', { class: 'dialog-actions' }, h('button', { type: 'button', onclick: closeDialog }, 'Cancel'), save));
  showDialog(form);
  showHints();
}

pages.reservations = params => {
  const notice = h('div');
  let subnets = [];
  let allGlobal = false;
  const table = dataTable({
    params,
    search: 'Search IP, MAC, vendor, hostname or comment',
    empty: 'No reservations',
    initialSort: 'ipAddress',
    filters: [{
      key: 'subnet', label: 'All subnets',
      options: rows => [...new Set(rows.map(r => r.subnet))].sort().map(v => ({ value: v, label: v })),
      match: (r, v) => r.subnet === v,
    }],
    columns: [
      {
        key: 'ipAddress', label: 'IP', class: 'mono', sort: r => ipKey(r.ipAddress), search: r => r.ipAddress,
        render: r => [r.ipAddress || '–', r.inPool ? ' ' : null, r.inPool ? h('span', { class: 'tag', title: 'Address is inside the dynamic pool' }, 'in pool') : null],
      },
      { key: 'identifier', label: 'Identifier', class: 'mono', search: r => `${r.identifier} ${r.vendor || ''}`, render: r => [r.identifierType === 'hw-address' ? null : h('span', { class: 'tag' }, r.identifierType), r.identifierType === 'hw-address' ? null : ' ', macCell(r.identifier, r.vendor)] },
      { key: 'hostname', label: 'Hostname', search: r => r.hostname },
      {
        key: 'lease', label: 'Lease', sort: r => r.lease?.remaining ?? -1,
        render: r => r.lease ? h('a', { href: searchLink('leases', r.lease.ipAddress), title: r.lease.hwAddress }, state(leaseStateKind[r.lease.state] || 'warn', `${r.lease.state} ${fmtDuration(r.lease.remaining)}`)) : '–',
      },
      { key: 'subnet', label: 'Subnet', class: 'mono', render: r => [r.subnet === 'global' ? null : r.subnet, r.global ? ' ' : null, r.global ? h('span', { class: 'tag', title: 'Global reservation (subnet ID 0)' }, 'global') : null] },
      {
        key: 'options', label: 'Options', class: 'mono', sort: r => r.options.length,
        search: r => r.options.map(o => o.name + ' ' + o.data).join(' '),
        render: r => r.options.length
          ? h('span', { class: 'tag', title: r.options.map(o => `${o.name || o.code}=${o.data}`).join('\n') }, String(r.options.length))
          : '–',
      },
      { key: 'classes', label: 'Classes', search: r => r.classes.join(' '), sort: r => r.classes.join(' '), render: r => r.classes.length ? classTags(r.classes) : '–' },
      { key: 'comment', label: 'Comment', search: r => r.comment },
    ],
    actions: meta.writable ? [
      r => h('button', { class: 'small', onclick: () => reservationDialog(subnets, r, load) }, 'Edit'),
      r => h('button', {
        class: 'small danger',
        onclick: () => confirmDialog('Delete reservation', `Really delete reservation ${r.ipAddress || ''} (${r.hostname || r.identifier})?`, 'Delete',
          async () => showResult(await api('DELETE', reservationPath(r)), load)),
      }, 'Delete'),
    ] : [],
  });
  const load = async () => {
    try {
      const [list, net] = await Promise.all([api('GET', 'reservations'), api('GET', 'subnets')]);
      subnets = net.subnets;
      table.setRows(list.reservations);
      allGlobal = list.reservations.length > 0 && list.reservations.every(r => r.global);
      notice.replaceChildren(...[notes(list.warnings)].filter(Boolean));
    } catch (err) {
      notice.replaceChildren(errorNote(err));
    }
  };
  view.replaceChildren(h('div', { class: 'page-head' }, h('h1', {}, 'Reservations'),
    meta.writable ? h('button', { class: 'primary', onclick: () => reservationDialog(subnets, null, load, allGlobal) }, 'New reservation') : null,
    h('button', { onclick: load }, 'Refresh')), notice, table.el);
  load();
};

// Age of a device's last ARP packet. "current" allows for missing a few runs of
// an active scan of the monitor.
const SEEN_CURRENT = 15 * 60e3;
const SEEN_RECENT = 86400e3;

function seenState(row) {
  const age = Date.now() - new Date(row.lastSeen).getTime();
  if (age < SEEN_CURRENT) return { key: 'current', kind: 'ok', label: 'current' };
  if (age < SEEN_RECENT) return { key: 'recent', kind: 'neutral', label: 'recent' };
  return { key: 'stale', kind: 'warn', label: 'stale' };
}

function fmtAge(t) {
  return fmtDuration(Math.floor((Date.now() - new Date(t).getTime()) / 1000)) + ' ago';
}

pages.devices = params => {
  const notice = h('div');
  const table = dataTable({
    params,
    search: 'Search IP, MAC, vendor or name',
    empty: 'No devices',
    initialSort: 'lastSeen',
    rowClass: r => (seenState(r).key === 'stale' ? 'stale' : ''),
    filters: [{
      key: 'reserved', label: 'With and without reservation', default: 'unreserved',
      options: () => [{ value: 'unreserved', label: 'Unreserved only' }, { value: 'reserved', label: 'Reserved only' }],
      // a conflict needs attention, so it never hides behind a reservation
      match: (r, v) => (r.reservation && !r.reservation.conflict ? 'reserved' : 'unreserved') === v,
    }, {
      key: 'seen', label: 'Any age',
      options: () => [{ value: 'current', label: 'Current' }, { value: 'recent', label: 'Recent' }, { value: 'stale', label: 'Stale' }],
      match: (r, v) => seenState(r).key === v,
    }],
    columns: [
      {
        key: 'ipAddress', label: 'IP', class: 'mono', sort: r => ipKey(r.ipAddress), search: r => `${r.ipAddress} ${r.subnet}`,
        render: r => [clientLink(r.ipAddress), r.inPool ? ' ' : null, r.inPool ? h('span', { class: 'tag', title: 'Address is inside the dynamic pool' }, 'in pool') : null],
      },
      { key: 'hwAddress', label: 'MAC', class: 'mono', search: r => `${r.hwAddress} ${r.vendor || ''}`, render: r => macCell(r.hwAddress, r.vendor) },
      { key: 'hostname', label: 'DNS name', search: r => r.hostname },
      {
        key: 'lease', label: 'Lease', sort: r => r.lease?.remaining ?? -1,
        render: r => r.lease ? h('a', { href: searchLink('leases', r.ipAddress) }, state(leaseStateKind[r.lease.state] || 'warn', r.lease.state)) : '–',
      },
      {
        key: 'reservation', label: 'Reservation', class: 'mono wrap', sort: r => r.reservation ? 1 : 0,
        search: r => (r.reservation?.classes ?? []).join(' '),
        render: reservationCell,
      },
      { key: 'wifi', label: 'Where', class: 'mono', search: r => `${r.wifi?.accessPoint ?? ''} ${r.wifi?.interface ?? ''}`, sort: r => r.wifi?.accessPoint ?? '', render: r => whereCell(r.wifi, true) },
      { key: 'subnet', label: 'Subnet', class: 'mono' },
      {
        key: 'seen', label: 'Seen', sort: r => ['current', 'recent', 'stale'].indexOf(seenState(r).key),
        render: r => { const s = seenState(r); return state(s.kind, s.label); },
      },
      { key: 'lastSeen', label: 'Last seen', class: 'mono', sort: r => -new Date(r.lastSeen).getTime(), render: r => h('span', { title: fmtTime(r.lastSeen) }, fmtAge(r.lastSeen)) },
      { key: 'firstSeen', label: 'First seen', class: 'mono', sort: r => r.firstSeen ? new Date(r.firstSeen).getTime() : 0, render: r => r.firstSeen ? h('span', { title: fmtTime(r.firstSeen) }, fmtShort(r.firstSeen)) : '–' },
    ],
    actions: meta.writable ? [r => r.reservation ? null : h('button', {
      class: 'small',
      title: 'Create a reservation for this device',
      onclick: async () => {
        try {
          await reserveLease(r, load);
        } catch (err) {
          notice.replaceChildren(errorNote(err));
        }
      },
    }, 'Reserve')] : [],
  });
  const load = async () => {
    try {
      const list = await api('GET', 'devices');
      table.setRows(list.devices);
      notice.replaceChildren(...[notes(list.warnings)].filter(Boolean));
    } catch (err) {
      notice.replaceChildren(errorNote(err));
    }
  };
  view.replaceChildren(h('div', { class: 'page-head' }, h('h1', {}, 'Devices'), refreshControls(load)), notice, table.el);
  load();
};

pages.subnets = () => {
  const body = h('div', {});
  const load = async () => {
    try {
      const list = await api('GET', 'subnets');
      body.replaceChildren(...[notes(list.warnings), ...list.subnets.map(s => subnetCard(s, list.editable, load))].filter(Boolean));
      if (!list.subnets.length) body.append(h('div', { class: 'panel empty' }, 'No subnets configured'));
    } catch (err) {
      body.replaceChildren(errorNote(err));
    }
  };
  view.replaceChildren(h('div', { class: 'page-head' }, h('h1', {}, 'Subnets'), h('button', { onclick: load }, 'Refresh')), body);
  load();
};

function classDialog(existing, reload) {
  const name = h('input', { class: 'mono-input', required: true, disabled: !!existing, value: existing?.name || '' });
  const options = h('div', {});
  (existing?.options || [{}]).forEach(o => optionRow(options, o));
  const error = h('div');
  const save = h('button', { class: 'primary', type: 'submit' }, existing ? 'Save' : 'Create');
  showDialog(h('form', {
    onsubmit: async ev => {
      ev.preventDefault();
      save.disabled = true;
      error.replaceChildren();
      const body = { name: name.value.trim(), options: [...options.children].map(row => row.read()) };
      try {
        showResult(existing
          ? await api('PUT', 'classes/' + encodeURIComponent(existing.name), body)
          : await api('POST', 'classes', body), reload);
      } catch (err) {
        error.replaceChildren(h('div', { class: 'notes' }, (err.problems || [err.message]).map(p => h('div', { class: 'note crit' }, p))));
        save.disabled = false;
      }
    },
  },
    h('h2', {}, existing ? `Edit class ${existing.name}` : 'Create class'),
    field('Name', name),
    h('div', { class: 'field' }, h('label', {}, 'DHCP options'), options,
      h('div', {}, h('button', { type: 'button', class: 'small', onclick: () => optionRow(options, {}) }, 'Add option'))),
    error,
    h('div', { class: 'dialog-actions' }, h('button', { type: 'button', onclick: closeDialog }, 'Cancel'), save)));
}

pages.classes = params => {
  const notice = h('div');
  let editable = false;
  const table = dataTable({
    params,
    search: 'Search class or option',
    empty: 'No client classes',
    initialSort: 'name',
    columns: [
      { key: 'name', label: 'Class', class: 'mono', search: r => r.name },
      {
        key: 'options', label: 'Options', class: 'mono', noSort: true,
        search: r => r.options.map(o => `${o.name} ${o.data}`).join(' '),
        render: r => r.options.length ? r.options.map(o => h('div', {}, `${o.name || o.code}=${o.data}`)) : '–',
      },
      { key: 'test', label: 'Test', class: 'mono' },
      { key: 'reservations', label: 'Reservations', class: 'mono', render: r => h('a', { href: searchLink('reservations', r.name) }, String(r.reservations)) },
    ],
    actions: meta.writable ? [
      r => editable ? h('button', { class: 'small', onclick: () => classDialog(r, load) }, 'Edit') : null,
      r => editable && !r.reservations ? h('button', {
        class: 'small danger',
        onclick: () => confirmDialog('Delete class', `Really delete class ${r.name}?`, 'Delete',
          async () => showResult(await api('DELETE', 'classes/' + encodeURIComponent(r.name)), load)),
      }, 'Delete') : null,
    ] : [],
  });
  const load = async () => {
    try {
      const list = await api('GET', 'classes');
      editable = list.editable;
      add.hidden = !editable;
      table.setRows(list.classes);
      notice.replaceChildren();
    } catch (err) {
      notice.replaceChildren(errorNote(err));
    }
  };
  const add = h('button', { class: 'primary', hidden: true, onclick: () => classDialog(null, load) }, 'New class');
  view.replaceChildren(h('div', { class: 'page-head' }, h('h1', {}, 'Client classes'), meta.writable ? add : null, h('button', { onclick: load }, 'Refresh')), notice, table.el);
  load();
};

function splitList(text) {
  return text.split(',').map(v => v.trim()).filter(Boolean);
}

function poolRow(list, value) {
  const input = h('input', { class: 'mono-input', placeholder: '192.0.2.100-192.0.2.199', 'aria-label': 'Pool', value });
  const row = h('div', { class: 'pool-row' }, input, h('button', { type: 'button', class: 'small', onclick: () => row.remove() }, '×'));
  row.read = () => input.value.trim();
  list.append(row);
}

function subnetDialog(s, reload) {
  const pools = h('div', {});
  s.pools.forEach(p => poolRow(pools, p.pool));
  const gateways = h('input', { class: 'mono-input', value: (s.gateways || []).join(', ') });
  const dns = h('input', { class: 'mono-input', value: (s.dnsServers || []).join(', ') });
  const lifetime = h('input', { class: 'mono-input', type: 'number', min: '1', value: s.validLifetime ?? '' });
  const error = h('div');
  const save = h('button', { class: 'primary', type: 'submit' }, 'Save');
  const show = list => error.replaceChildren(h('div', { class: 'notes' }, list.map(p => h('div', { class: 'note crit' }, p))));
  const input = () => ({
    pools: [...pools.children].map(row => row.read()).filter(Boolean),
    gateways: splitList(gateways.value),
    dnsServers: splitList(dns.value),
    validLifetime: Number(lifetime.value),
  });
  const apply = async body => showResult(await api('PUT', 'subnets/' + s.id, body), reload);
  const form = h('form', {
    onsubmit: async ev => {
      ev.preventDefault();
      save.disabled = true;
      error.replaceChildren();
      const body = input();
      try {
        const check = await api('POST', `subnets/${s.id}/check`, body);
        if (!check.warnings.length) {
          await apply(body);
          return;
        }
        // nothing is applied before the operator has seen the consequences
        showDialog(h('h2', {}, 'Apply this change?'),
          h('div', { class: 'notes' }, check.warnings.map(w => h('div', { class: 'note warn' }, w))),
          h('div', { class: 'dialog-actions' },
            h('button', { onclick: closeDialog }, 'Cancel'),
            h('button', { class: 'danger solid', onclick: () => apply(body).catch(err => dialog.append(errorNote(err))) }, 'Apply anyway')));
      } catch (err) {
        show(err.problems || [err.message]);
        save.disabled = false;
      }
    },
  },
    h('h2', {}, `Edit subnet ${s.subnet}`),
    h('div', { class: 'field' }, h('label', {}, 'Pools'), pools,
      h('div', {}, h('button', { type: 'button', class: 'small', onclick: () => poolRow(pools, '') }, 'Add pool'))),
    field('Gateway', gateways),
    field('DNS servers', dns),
    field('Lease lifetime (seconds)', lifetime),
    error,
    h('div', { class: 'dialog-actions' }, h('button', { type: 'button', onclick: closeDialog }, 'Cancel'), save));
  showDialog(form);
}

function subnetCard(s, editable, reload) {
  const dl = (key, value) => [h('dt', {}, key), h('dd', {}, value)];
  const list = v => v && v.length ? v.join(', ') : '–';
  return h('div', {},
    h('h2', {}, `Subnet ${s.id}`, meta.writable && editable && !s.sharedNetwork
      ? h('button', { class: 'small subnet-edit', onclick: () => subnetDialog(s, reload) }, 'Edit') : null),
    h('div', { class: 'panel pad' },
      h('div', { class: 'columns' },
        h('dl', { class: 'kv' },
          dl('CIDR', s.subnet),
          dl('Subnet ID', String(s.id)),
          s.sharedNetwork ? dl('Shared Network', s.sharedNetwork) : null,
          dl('Gateway', list(s.gateways)),
          dl('DNS', list(s.dnsServers)),
          dl('Lease lifetime', s.validLifetime != null ? fmtDuration(s.validLifetime) : '–'),
          s.outsidePools != null ? dl('Leases outside pools', fmtCount(s.outsidePools)) : null,
          dl('Reservations', h('a', { href: `#/reservations?subnet=${encodeURIComponent(s.subnet)}` }, fmtCount(s.reservations))),
          s.declined ? dl('Declined', fmtCount(s.declined)) : null,
          s.options.filter(o => !['routers', 'domain-name-servers'].includes(o.name)).map(o => dl(o.name || `Option ${o.code}`, o.data || '–'))),
        h('div', {}, s.pools.length
          ? s.pools.map(p => meterRow(p.pool, p.assigned, p.total, p.percent))
          : h('div', { class: 'empty' }, 'No pools')))));
}

pages.ha = () => {
  const body = h('div', {});
  const load = async () => {
    try {
      const d = await api('GET', 'dashboard');
      updateChip(d.server);
      const warnings = d.server.reachable ? d.haWarnings : d.warnings;
      const content = [notes(warnings), ...d.ha.map(haCard)].filter(Boolean);
      if (!d.ha.length && d.server.reachable) content.push(h('div', { class: 'panel empty' }, 'No HA status: hook libdhcp_ha is not loaded'));
      if (meta.writable && d.ha.length) {
        const { servers } = await api('GET', 'servers');
        content.push(h('h2', {}, 'Operate'), ...servers.filter(s => s.ha).map(s => haActions(s, load)));
      }
      body.replaceChildren(...content);
    } catch (err) {
      body.replaceChildren(errorNote(err));
    }
  };
  view.replaceChildren(h('div', { class: 'page-head' }, h('h1', {}, 'High availability'), refreshControls(load)), body);
  load();
};

const HA_ACTIONS = {
  maintenance: {
    label: 'Start maintenance',
    text: s => `${s.ha.partner} takes over all DHCP traffic and ${s.ha.name} stops serving. Afterwards ${s.ha.name} can be restarted or shut down safely.`,
  },
  'maintenance-cancel': { label: 'End maintenance', text: s => `${s.ha.name} and ${s.ha.partner} return to normal operation.` },
  sync: {
    label: 'Sync leases',
    text: s => `${s.ha.name} fetches all leases from ${s.ha.partner}. ${s.ha.partner} stops serving DHCP while this runs, for up to a minute.`,
  },
  reset: { label: 'Reset', text: s => `${s.ha.name} leaves the state ${s.ha.state}, goes back to waiting and synchronizes with ${s.ha.partner || 'its partner'}.` },
  continue: { label: 'Continue', text: s => `Resumes the HA state machine of ${s.ha.name} if it is paused.` },
};

function haActions(s, reload) {
  const path = action => `servers/${encodeURIComponent(s.name)}/ha/${action}`;
  return h('div', { class: 'panel pad server-card' },
    h('h3', {}, s.name), h('p', {}, state('neutral', s.ha.state)),
    s.ha.actions.length ? h('div', { class: 'ha-actions' }, s.ha.actions.map(a => h('button', {
      onclick: () => confirmDialog(HA_ACTIONS[a].label + ': ' + s.name, HA_ACTIONS[a].text(s), HA_ACTIONS[a].label,
        async () => showResult(await api('POST', path(a)), reload)),
    }, HA_ACTIONS[a].label))) : h('p', { class: 'muted' }, 'Nothing to do in this state'));
}

function haCard(ha) {
  const dl = (key, value) => [h('dt', {}, key), h('dd', {}, value)];
  const { local, remote } = ha['ha-servers'];
  const yes = (flag, bad) => flag === bad ? state('crit', flag ? 'yes' : 'no') : state('ok', flag ? 'yes' : 'no');
  return h('div', {},
    h('p', {}, h('span', { class: 'tag' }, ha['ha-mode'])),
    h('div', { class: 'columns' },
      h('div', { class: 'panel pad' },
        h('h3', {}, 'This server: ' + local['server-name']),
        h('dl', { class: 'kv' },
          dl('Role', local.role),
          dl('State', state(ha.severity, local.state)),
          dl('Scopes', (local.scopes || []).join(', ') || '–'),
          dl('System time (UTC)', local['system-time'] || '–'),
          dl('Unsent updates', fmtCount(ha.unsentUpdates)))),
      remote ? h('div', { class: 'panel pad' },
        h('h3', {}, 'Partner: ' + remote['server-name']),
        h('dl', { class: 'kv' },
          dl('Role', remote.role),
          dl('Last state', remote['last-state'] || '–'),
          dl('In touch', yes(remote['in-touch'], false)),
          dl('Communication interrupted', yes(remote['communication-interrupted'], true)),
          dl('Last contact', remote.age != null ? fmtDuration(remote.age) : '–'),
          dl('Clock skew', remote['clock-skew'] != null ? remote['clock-skew'] + ' s' : '–'),
          dl('Unacked clients', fmtCount(remote['unacked-clients'])))) : null));
}

pages.audit = () => {
  const notice = h('div');
  const audit = dataTable({
    search: 'Search user, action or target',
    empty: 'No changes yet',
    columns: [
      { key: 'time', label: 'Time', class: 'mono', render: r => fmtTime(r.time) },
      { key: 'user', label: 'User', class: 'mono', search: r => r.user },
      { key: 'action', label: 'Action', class: 'mono', search: r => r.action },
      { key: 'target', label: 'Target', class: 'mono', search: r => r.target },
      {
        key: 'status', label: 'Result',
        render: r => state({ ok: 'ok', failed: 'crit', partial: 'warn' }[r.status] || 'neutral',
          { ok: 'ok', failed: 'failed', partial: 'partial' }[r.status] || r.status),
      },
    ],
  });
  const backups = dataTable({
    search: 'Search backups',
    empty: 'No backups yet',
    columns: [
      { key: 'name', label: 'File', class: 'mono', search: r => r.name, render: r => h('a', { href: 'api/backups/' + encodeURIComponent(r.name), download: r.name }, r.name) },
      { key: 'size', label: 'Size', class: 'mono', render: r => fmtCount(r.size) + ' B' },
      { key: 'time', label: 'Time', class: 'mono', render: r => fmtTime(r.time) },
    ],
  });
  const load = async () => {
    try {
      const [entries, files] = await Promise.all([api('GET', 'audit'), api('GET', 'backups')]);
      audit.setRows(entries);
      backups.setRows(files);
    } catch (err) {
      notice.replaceChildren(errorNote(err));
    }
  };
  view.replaceChildren(h('div', { class: 'page-head' }, h('h1', {}, 'Audit log'), h('button', { onclick: load }, 'Refresh')),
    notice, audit.el, h('h2', {}, 'Configuration backups'), backups.el);
  load();
};

pages.servers = () => {
  const notice = h('div');
  const list = h('div');
  const load = async () => {
    try {
      const { servers } = await api('GET', 'servers');
      list.replaceChildren(...servers.map(s => serverCard(s, load)));
      notice.replaceChildren();
    } catch (err) {
      notice.replaceChildren(errorNote(err));
    }
  };
  view.replaceChildren(h('div', { class: 'page-head' }, h('h1', {}, 'Servers'), h('button', { onclick: load }, 'Refresh')),
    h('p', { class: 'muted' }, 'Changes run on the primary first, then on each peer. Reservations, classes and subnets are compared.'),
    notice, list);
  load();
};

function serverOverview(rows) {
  const table = dataTable({
    plain: true,
    columns: [
      { key: 'name', label: 'Server', class: 'mono', render: r => h('a', { href: '#/servers' }, r.name) },
      { key: 'role', label: 'Role' },
      { key: 'haState', label: 'State', render: r => r.reachable ? state(r.severity, r.haState || 'running') : state('crit', 'unreachable') },
      { key: 'version', label: 'Version', class: 'mono' },
      { key: 'uptime', label: 'Uptime', class: 'mono', render: r => r.uptime ? fmtDuration(r.uptime) : '–' },
      { key: 'activeLeases', label: 'Active leases', class: 'mono', render: r => r.activeLeases != null ? fmtCount(r.activeLeases) : '–' },
      {
        key: 'differences', label: 'Configuration',
        render: r => r.role === 'primary' ? state('ok', 'source')
          : r.differences == null ? state('neutral', 'unknown')
            : r.differences ? state('warn', `${r.differences} differ`) : state('ok', 'in sync'),
      },
    ],
  });
  table.setRows(rows);
  return table.el;
}

function serverState(s) {
  if (!s.reachable) return state('crit', 'unreachable');
  if (s.role === 'primary') return state('ok', 'source');
  return s.differences.length ? state('warn', s.differences.length === 1 ? '1 difference' : `${s.differences.length} differences`) : state('ok', 'in sync');
}

function serverCard(s, reload) {
  const syncable = s.differences.filter(d => !d.manual);
  const removed = syncable.filter(d => d.change === 'extra').length;
  const changeText = { missing: 'missing here', extra: 'only here', different: 'different' };
  return h('div', { class: 'panel pad server-card' },
    h('h3', {}, s.name),
    h('dl', { class: 'kv' },
      h('dt', {}, 'Role'), h('dd', {}, s.role),
      h('dt', {}, 'Version'), h('dd', {}, s.version || '–'),
      h('dt', {}, 'State'), h('dd', {}, serverState(s))),
    s.error ? h('div', { class: 'note crit' }, s.error) : null,
    s.differences.length ? h('ul', { class: 'steps' }, s.differences.map(d => h('li', {},
      state(d.change === 'extra' ? 'crit' : 'warn', changeText[d.change]),
      h('span', { class: 'detail' }, `${d.kind} ${d.target}${d.manual ? ' (fix in the configuration file)' : ''}`)))) : null,
    meta.writable && syncable.length ? h('button', {
      class: 'primary',
      onclick: () => confirmDialog('Copy from primary',
        `Make ${s.name} match the primary? ${syncable.length} entries change${removed ? `, ${removed} of them are deleted on ${s.name}` : ''}.`, 'Copy',
        async () => showResult(await api('POST', 'servers/' + encodeURIComponent(s.name) + '/sync'), reload)),
    }, 'Copy from primary') : null);
}

const SEVERITY_KIND = { info: 'neutral', ok: 'ok', warn: 'warn', crit: 'crit' };

function severityWord(severity) {
  return { info: 'note', ok: 'ok', warn: 'warning', crit: 'problem' }[severity] || severity;
}

function kvRows(rows) {
  return h('dl', { class: 'kv' }, rows.filter(Boolean).flatMap(([key, value]) => [h('dt', {}, key), h('dd', {}, value)]));
}

function clientCard(title, rows, empty, action) {
  return h('div', { class: 'client-card' }, h('h3', {}, title), rows ? kvRows(rows) : h('p', { class: 'muted' }, empty), action);
}

// Offers a reservation for what the popup knows about a client without one.
function reserveClient(c) {
  if (c.reservation || !meta.writable || !(c.hwAddress || c.lease?.clientId)) return null;
  const source = c.lease || { ipAddress: c.ipAddress, hwAddress: c.hwAddress, clientId: '', hostname: c.name || '' };
  const reopen = () => clientDialog(c.hwAddress || c.ipAddress);
  return h('button', {
    class: 'small card-action',
    onclick: () => reserveLease(source, reopen).catch(err => dialog.append(errorNote(err))),
  }, 'Reserve');
}

function closeButton() {
  return h('button', { class: 'dialog-close', title: 'Close', 'aria-label': 'Close', onclick: closeDialog }, '×');
}

function explainSteps(steps) {
  return h('ol', { class: 'explain' }, steps.map(s => h('li', { class: SEVERITY_KIND[s.severity] },
    h('span', { class: 'step-title' }, s.title), h('span', { class: 'step-detail' }, s.detail))));
}

function optionTable(options) {
  if (!options.length) return h('p', { class: 'muted' }, 'No options');
  return h('div', { class: 'table-wrap' }, h('table', {},
    h('thead', {}, h('tr', {}, h('th', {}, 'Option'), h('th', {}, 'Value'), h('th', {}, 'From'))),
    h('tbody', {}, options.map(o => h('tr', {},
      h('td', { class: 'mono' }, o.name),
      h('td', { class: 'mono wrap' }, o.data, o.overrides.map(x => h('div', { class: 'overridden', title: 'Overridden' }, `${x.data} (${x.source})`))),
      h('td', {}, h('span', { class: 'tag' }, o.source)))))));
}

function clientActions(c) {
  const actions = [];
  if (c.reservation) actions.push(h('a', { class: 'button', href: searchLink('reservations', c.reservation.identifier) }, 'Open reservation'));
  if (c.lease) actions.push(h('a', { class: 'button', href: searchLink('leases', c.lease.ipAddress) }, 'Open lease'));
  if (meta.devices && c.device) actions.push(h('a', { class: 'button', href: searchLink('devices', c.hwAddress) }, 'Open device'));
  return actions.length ? h('div', { class: 'dialog-actions' }, actions) : null;
}

function clientBody(c) {
  const l = c.lease;
  const r = c.reservation;
  const d = c.device;
  const w = c.wifi;
  return [
    closeButton(),
    h('h2', {}, c.name || c.hwAddress || c.ipAddress),
    h('p', { class: 'client-sub' }, [c.hwAddress, c.ipAddress, c.vendor].filter(Boolean).join(' · ')),
    notes(c.warnings.map(message => ({ severity: 'warn', message }))),
    h('div', { class: 'client-grid' },
      clientCard('Lease', l && [
        ['Address', l.ipAddress],
        ['State', state(leaseStateKind[l.state] || 'warn', l.state)],
        ['Contact', fmtShort(l.started)],
        ['Remaining', fmtDuration(l.remaining)],
      ], 'No lease'),
      clientCard('Reservation', r && [
        ['Address', r.ipAddress || '–'],
        ['Hostname', r.hostname || '–'],
        ['Scope', r.subnetId ? `subnet ${r.subnetId}` : 'global'],
        r.classes.length ? ['Classes', classTags(r.classes)] : null,
        r.comment ? ['Comment', r.comment] : null,
      ], 'No reservation', reserveClient(c)),
      clientCard('Network', d && [
        ['Last seen', fmtAge(d.lastSeen)],
        d.firstSeen ? ['First seen', fmtShort(d.firstSeen)] : null,
        ['Addresses', d.addresses.length ? d.addresses.map(ip => h('div', {}, ip)) : '–'],
        d.hostname ? ['DNS name', d.hostname] : null,
      ], 'Not seen'),
      w ? clientCard('WiFi', [['Access point', w.accessPoint], ['Network', w.interface], ['Signal', `${w.signal} dBm`], ['Connected', fmtDuration(w.connectedTime)]]) : null),
    h('h3', {}, c.explanation.static ? 'Why it has this address' : 'Why it gets this address'),
    explainSteps(c.explanation.steps),
    c.explanation.unknown.length ? h('div', { class: 'notes' }, h('div', { class: 'note' },
      h('strong', {}, 'Depends on what the client sends: '), c.explanation.unknown.join(' '))) : null,
    h('h3', {}, c.explanation.static ? 'Options it would get over DHCP' : 'Options it receives'),
    optionTable(c.explanation.options),
    c.history.length ? h('h3', {}, 'Changes') : null,
    c.history.length ? h('ul', { class: 'history' }, c.history.map(a => h('li', {},
      h('span', { class: 'mono' }, fmtShort(a.time)), ' ',
      state({ ok: 'ok', failed: 'crit', partial: 'warn' }[a.status] || 'neutral', a.action), ' ',
      h('span', { class: 'muted' }, a.user)))) : null,
    clientActions(c),
  ];
}

async function clientDialog(id) {
  showDialog(closeButton(), h('h2', {}, id), h('p', { class: 'muted' }, 'Loading…'));
  dialog.classList.add('wide');
  const opened = dialogGeneration;
  // closing it, or opening something else, while it loads wins over a late answer
  const current = () => opened === dialogGeneration && dialog.open;
  try {
    const c = await api('GET', 'clients/' + encodeURIComponent(id));
    if (!current()) return;
    showDialog(...clientBody(c).filter(Boolean));
    dialogGeneration = opened;
    dialog.classList.add('wide');
  } catch (err) {
    if (current()) showDialog(closeButton(), h('h2', {}, id), errorNote(err));
  }
}

function funnelRow(label, value, peak) {
  const fill = h('div', {});
  fill.style.width = (peak ? value / peak * 100 : 0) + '%';
  return h('div', { class: 'funnel-row' }, h('span', { class: 'name' }, label), h('div', { class: 'meter' }, fill), h('span', { class: 'num' }, fmtCount(value)));
}

function trafficCard(t) {
  const peak = Math.max(t.discover, t.offer, t.request, t.ack, 1);
  const trouble = [['NAK', t.nak], ['Declined', t.decline], ['Dropped', t.dropped], ['Malformed', t.parseFailed], ['No address', t.allocFailed], ['Conflicts', t.conflicts]];
  return h('div', { class: 'panel pad' },
    h('h3', {}, t.server, t.standby ? h('span', { class: 'tag', title: 'A standby leaves requests to the primary' }, 'standby') : null),
    h('p', { class: 'muted' }, `Since Kea started ${fmtDuration(t.uptime)} ago: ${fmtCount(t.received)} received, ${fmtCount(t.sent)} sent`),
    funnelRow('Discover', t.discover, peak), funnelRow('Offer', t.offer, peak), funnelRow('Request', t.request, peak), funnelRow('Ack', t.ack, peak),
    h('div', { class: 'counters' }, trouble.map(([name, value]) => h('div', { class: value ? 'counter warn' : 'counter' }, h('b', {}, fmtCount(value)), h('span', {}, name)))));
}

function checkPanel(title, checks) {
  return h('div', { class: 'panel pad' }, h('h3', {}, title),
    h('ul', { class: 'checks' }, checks.map(c => h('li', {}, state(SEVERITY_KIND[c.severity], c.name), h('span', { class: 'detail' }, c.detail)))));
}

// Leads from a finding to its entry.
function findingLink(f) {
  if (f.page === 'client') return clientLink(f.query, f.target);
  if (!f.page) return f.target;
  return h('a', { href: f.query ? searchLink(f.page, f.query) : '#/' + f.page }, f.target);
}

function checkLabel(c) {
  return c.server ? `${c.server}: ${c.name}` : c.name;
}

// The number on top opens the list behind it, problems first.
function checkSummary(checks, open, expanded) {
  const rank = { crit: 0, warn: 1, info: 2, ok: 3 };
  const sorted = checks.toSorted((a, b) => rank[a.severity] - rank[b.severity]);
  const passed = checks.filter(c => c.severity === 'ok').length;
  const text = open ? `${open} open points, ${passed} checks passed` : `${passed} checks passed`;
  const details = h('details', { class: 'check-list' },
    h('summary', { title: sorted.map(c => `${checkLabel(c)}: ${c.detail}`).join('\n') }, text),
    h('ul', { class: 'checks' }, sorted.map(c => h('li', {}, state(SEVERITY_KIND[c.severity], checkLabel(c)), h('span', { class: 'detail' }, c.detail)))));
  details.open = expanded;
  return details;
}

// Shows what the review looks at, also while it finds nothing.
function ruleSummary(rules, expanded, remember) {
  const run = rules.filter(r => !r.skipped);
  const hits = run.filter(r => r.findings).length;
  const hiddenCount = run.reduce((sum, r) => sum + (r.hidden || 0), 0);
  const result = r => r.skipped ? state('neutral', r.skipped)
    : r.findings ? state('warn', r.findings === 1 ? '1 finding' : `${r.findings} findings`)
      : r.hidden ? state('neutral', `${r.hidden} hidden`) : state('ok', 'nothing found');
  const details = h('details', { class: 'check-list rule-list' },
    h('summary', {}, `${run.length} rules checked, ${hits} with findings${hiddenCount ? `, ${hiddenCount} hidden` : ''}`),
    h('ul', { class: 'checks' }, rules.map(r => h('li', {}, result(r), h('span', { class: 'detail' }, r.title)))));
  details.open = expanded;
  details.addEventListener('toggle', () => remember(details.open));
  return details;
}

pages.health = () => {
  const body = h('div', {});
  let listOpen = false;
  let rulesOpen = false;
  const findings = dataTable({
    search: 'Search findings',
    empty: 'Nothing to report',
    filters: [{
      key: 'hidden', label: 'All findings', default: 'shown',
      options: () => [{ value: 'shown', label: 'Open findings' }, { value: 'hidden', label: 'Hidden findings' }],
      match: (r, v) => (r.hidden ? 'hidden' : 'shown') === v,
    }],
    actions: meta.writable ? [r => h('button', {
      class: 'small',
      title: r.hidden ? 'Show this finding again' : 'This is intended: stop reporting it',
      onclick: () => hideFinding(r, !r.hidden),
    }, r.hidden ? 'Show' : 'Hide')] : [],
    columns: [
      {
        key: 'severity', label: 'Level', sort: r => ['crit', 'warn', 'info'].indexOf(r.severity),
        render: r => state(SEVERITY_KIND[r.severity], severityWord(r.severity)),
      },
      { key: 'area', label: 'Area', search: r => r.area },
      { key: 'target', label: 'Entry', class: 'mono', search: r => r.target, render: findingLink },
      { key: 'message', label: 'Finding', class: 'wrap', search: r => r.message },
    ],
    initialSort: 'severity',
  });
  const hideFinding = async (f, hidden) => {
    try {
      await api('POST', 'findings/hide', { rule: f.rule, target: f.target, hidden });
      load();
    } catch (err) {
      body.prepend(errorNote(err));
    }
  };
  const load = async () => {
    try {
      const r = await api('GET', 'health');
      const groups = new Map();
      for (const c of r.checks) groups.set(c.server || 'General', [...(groups.get(c.server || 'General') || []), c]);
      const count = list => list.filter(x => x.severity === 'warn' || x.severity === 'crit').length;
      const open = count(r.checks) + count(r.findings.filter(f => !f.hidden));
      findings.setRows(r.findings);
      const details = checkSummary(r.checks, open, listOpen);
      details.addEventListener('toggle', () => { listOpen = details.open; });
      body.replaceChildren(
        h('div', { class: 'health-summary ' + SEVERITY_KIND[r.severity] },
          state(SEVERITY_KIND[r.severity], r.severity === 'ok' ? 'All good' : r.severity === 'crit' ? 'Needs attention' : 'Worth a look'),
          details),
        h('div', { class: 'columns' }, [...groups].map(([name, checks]) => checkPanel(name, checks))),
        h('h2', {}, 'DHCP traffic'),
        r.traffic.length ? h('div', { class: 'columns' }, r.traffic.map(trafficCard)) : h('div', { class: 'panel empty' }, 'No statistics'),
        h('h2', {}, 'Configuration review'),
        ruleSummary(r.rules, rulesOpen, value => { rulesOpen = value; }),
        findings.el);
    } catch (err) {
      body.replaceChildren(errorNote(err));
    }
  };
  view.replaceChildren(h('div', { class: 'page-head' }, h('h1', {}, 'Health'), refreshControls(load)), body);
  load();
};

pages.settings = () => {
  const notice = h('div');
  const groups = h('div', {});
  const form = h('form', {
    onsubmit: async event => {
      event.preventDefault();
      const values = {};
      for (const input of form.querySelectorAll('input')) values[input.name] = Number(input.value);
      try {
        draw(await api('PUT', 'settings', values));
        notice.replaceChildren(h('div', { class: 'note ok' }, 'Settings saved.'));
      } catch (err) {
        notice.replaceChildren(errorNote(err));
      }
    },
  }, groups, h('div', { class: 'dialog-actions' }, h('button', { class: 'primary', type: 'submit' }, 'Save')));
  const draw = fields => {
    const names = [...new Set(fields.map(f => f.group))];
    groups.replaceChildren(...names.map(name => h('div', { class: 'panel pad setting-group' }, h('h3', {}, name),
      fields.filter(f => f.group === name).map(f => h('label', { class: 'setting' },
        h('span', { class: 'setting-name' }, f.label, f.changed ? h('span', { class: 'tag', title: `Default: ${f.default}` }, 'changed') : null),
        h('span', { class: 'setting-value' },
          h('input', { class: 'mono-input', type: 'number', name: f.key, min: String(f.min), max: String(f.max), value: f.value, required: true }), ' ', f.unit),
        h('span', { class: 'muted setting-help' }, `${f.help} Default: ${f.default}.`))))));
  };
  view.replaceChildren(h('div', { class: 'page-head' }, h('h1', {}, 'Settings')), notice, form);
  api('GET', 'settings').then(draw).catch(err => notice.replaceChildren(errorNote(err)));
};

function updateChip(server) {
  document.getElementById('server-chip').replaceChildren(
    server.reachable ? state('ok', server.name) : state('crit', server.name + ' offline'));
}

async function route() {
  stopRefresh();
  if (dialog.open) closeDialog();
  const [name, query] = location.hash.replace('#/', '').split('?');
  const page = pages[name] ? name : 'dashboard';
  for (const a of document.querySelectorAll('nav a')) a.classList.toggle('active', a.dataset.page === page);
  await loadClassOptions();
  pages[page](new URLSearchParams(query));
}

function themeControls() {
  const bind = (id, key, value) => {
    const select = document.getElementById(id);
    select.value = value;
    select.addEventListener('change', () => {
      localStorage.setItem(key, select.value);
      window.applyTheme();
    });
  };
  bind('theme-select', 'theme', document.documentElement.dataset.theme);
  bind('mode-select', 'mode', localStorage.getItem('mode') || 'auto');
}

async function start() {
  themeControls();
  try {
    meta = await api('GET', 'meta');
  } catch (err) {
    view.replaceChildren(errorNote(err));
    return;
  }
  const groups = [
    [['dashboard', 'Dashboard']],
    [['leases', 'Leases'], ['reservations', 'Reservations'], meta.devices && ['devices', 'Devices']],
    [['classes', 'Classes'], ['subnets', 'Subnets']],
    [['ha', 'HA'], meta.peers && ['servers', 'Servers']],
    [['health', 'Health'], meta.writable && ['audit', 'Audit log'], meta.writable && ['settings', 'Settings']],
  ];
  document.getElementById('nav').replaceChildren(...groups.map(group => h('div', { class: 'nav-group' },
    group.filter(Boolean).map(([page, label]) => h('a', { href: '#/' + page, 'data-page': page }, label)))));
  document.querySelector('.brand').title = 'Kea Roost ' + meta.version;
  document.getElementById('server-chip').replaceChildren(meta.writable ? '' : h('span', { class: 'tag' }, 'read-only'));
  window.addEventListener('hashchange', route);
  route();
}

start();
