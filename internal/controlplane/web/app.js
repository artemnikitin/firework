const content = document.querySelector('#content');
const toolbar = document.querySelector('#toolbar');
const search = document.querySelector('#search');
const filter = document.querySelector('#filter');
const updated = document.querySelector('#updated');
const errorBox = document.querySelector('#error');
const navButtons = [...document.querySelectorAll('[data-view]')];

const states = {
  nodes: ['ready', 'draining', 'down', 'stale', 'unknown'],
  services: ['pending', 'running', 'stopped', 'failed', 'unknown'],
};

let view = 'overview';
let data = [];
let requestedFilter = '';

const esc = value => String(value ?? '').replace(/[&<>'"]/g, character => ({
  '&': '&amp;',
  '<': '&lt;',
  '>': '&gt;',
  "'": '&#39;',
  '"': '&quot;',
}[character]));

const badge = value => `<span class="badge ${esc(value)}">${esc(value || 'unknown')}</span>`;
const display = value => value === undefined || value === null || value === '' ? '<span class="muted">—</span>' : esc(value);
const formatDate = value => value ? new Date(value).toLocaleString() : '<span class="muted">—</span>';
const formatBoolean = value => value ? 'Yes' : 'No';
const nodeLink = node => node ? `<a href="#node/${encodeURIComponent(node)}">${esc(node)}</a>` : '<span class="muted">—</span>';
const serviceLink = service => `<a href="#service/${encodeURIComponent(service)}">${esc(service)}</a>`;
const capacity = (used, total, available, unit = '') => {
  const numericUsed = Math.max(0, Number(used) || 0);
  const numericTotal = Math.max(0, Number(total) || 0);
  const numericAvailable = Math.max(0, Number(available) || 0);
  const progressMax = Math.max(1, numericTotal);
  const progressValue = Math.min(numericUsed, progressMax);
  return `<div class="capacity-value">
    <span>${esc(numericUsed)}/${esc(numericTotal)}${esc(unit)}</span>
    <progress max="${esc(progressMax)}" value="${esc(progressValue)}" aria-label="${esc(numericUsed)} of ${esc(numericTotal)}${esc(unit)} allocated"></progress>
    <span class="muted">${esc(numericAvailable)}${esc(unit)} available</span>
  </div>`;
};

async function api(path) {
  const response = await fetch(path, {credentials: 'same-origin'});
  if (response.status === 401) {
    location = '/login';
    throw new Error('Authentication required');
  }
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new Error(body.error || `HTTP ${response.status}`);
  }
  return response.json();
}

function setActiveView(nextView) {
  view = nextView;
  toolbar.hidden = !['nodes', 'services'].includes(view);
  navButtons.forEach(button => {
    const active = button.dataset.view === view;
    button.classList.toggle('active', active);
    button.setAttribute('aria-current', active ? 'page' : 'false');
  });
}

function setFilters() {
  const values = states[view] || [];
  const selected = requestedFilter || filter.value;
  filter.innerHTML = '<option value="">All states</option>' + values.map(value => `<option value="${value}">${value}</option>`).join('');
  filter.value = values.includes(selected) ? selected : '';
  requestedFilter = '';
}

function table(headers, rows, emptyMessage) {
  if (!rows.length) {
    return `<div class="empty-state">${esc(emptyMessage)}</div>`;
  }
  return `<div class="table-wrap"><table><thead><tr>${headers.map(header => `<th scope="col">${esc(header)}</th>`).join('')}</tr></thead><tbody>${rows.join('')}</tbody></table></div>`;
}

function detailTable(rows) {
  return `<div class="table-wrap"><table class="detail-table"><tbody>${rows.map(([label, value]) => `<tr><th scope="row">${esc(label)}</th><td>${value}</td></tr>`).join('')}</tbody></table></div>`;
}

function section(title, body) {
  return `<section class="detail-section"><h2>${esc(title)}</h2>${body}</section>`;
}

function renderList() {
  const query = search.value.trim().toLowerCase();
  const state = filter.value;
  const items = data.filter(item => {
    const name = item.node_id || item.name || '';
    return name.toLowerCase().includes(query) && (!state || item.state === state);
  });

  if (view === 'nodes') {
    const rows = items.map(node => `<tr>
      <td>${nodeLink(node.node_id)}</td>
      <td>${badge(node.state)}</td>
      <td>${node.last_seen_at ? formatDate(node.last_seen_at) : '<span class="muted">Never</span>'}</td>
      <td>${node.running_services}/${node.desired_services}</td>
      <td>${capacity(node.allocated.vcpus, node.capacity.vcpus, node.available.vcpus)}</td>
      <td>${capacity(node.allocated.memory_mb, node.capacity.memory_mb, node.available.memory_mb, ' MB')}</td>
    </tr>`);
    content.innerHTML = `<div class="page-heading"><div><p class="eyebrow">Infrastructure</p><h1>Nodes</h1></div><span class="result-count">${items.length} of ${data.length}</span></div>${table(['Node', 'State', 'Last seen', 'Services', 'vCPU', 'Memory'], rows, 'No nodes match the current filters.')}`;
    return;
  }

  const rows = items.map(service => `<tr>
    <td>${serviceLink(service.name)}</td>
    <td>${nodeLink(service.node)}</td>
    <td>${badge(service.state)}</td>
    <td>${badge(service.health)}</td>
    <td>${service.vcpus}</td>
    <td>${service.memory_mb} MB</td>
  </tr>`);
  content.innerHTML = `<div class="page-heading"><div><p class="eyebrow">Workloads</p><h1>Services</h1></div><span class="result-count">${items.length} of ${data.length}</span></div>${table(['Service', 'Node', 'State', 'Health', 'vCPU', 'Memory'], rows, 'No services match the current filters.')}`;
}

function stateCounts(items, expectedStates) {
  const counts = Object.fromEntries(expectedStates.map(state => [state, 0]));
  items.forEach(item => {
    const state = expectedStates.includes(item.state) ? item.state : 'unknown';
    counts[state] += 1;
  });
  return counts;
}

function overviewPanel(kind, items) {
  const counts = stateCounts(items, states[kind]);
  const title = kind === 'nodes' ? 'Nodes' : 'Services';
  const eyebrow = kind === 'nodes' ? 'Infrastructure' : 'Workloads';
  const cards = states[kind].map(state => `<button class="state-card" type="button" data-target="${kind}" data-state="${state}">
    <span class="state-card-label">${badge(state)}<span>${esc(state)}</span></span>
    <strong>${counts[state]}</strong>
  </button>`).join('');
  return `<section class="overview-panel">
    <div class="overview-panel-heading">
      <div><p class="eyebrow">${eyebrow}</p><h2>${title}</h2></div>
      <button class="total-metric" type="button" data-target="${kind}"><strong>${items.length}</strong><span>Total</span></button>
    </div>
    <div class="state-grid">${cards}</div>
  </section>`;
}

function renderOverview(nodes, services) {
  content.innerHTML = `<div class="overview-heading">
    <p class="eyebrow">Firework deployment</p>
    <h1>Platform overview</h1>
    <p>Current node availability and service lifecycle state across the platform.</p>
  </div>
  <div class="overview-grid">
    ${overviewPanel('nodes', nodes)}
    ${overviewPanel('services', services)}
  </div>`;

  content.querySelectorAll('[data-target]').forEach(button => {
    button.addEventListener('click', () => {
      requestedFilter = button.dataset.state || '';
      location.hash = button.dataset.target;
    });
  });
}

function renderNodeDetail(node) {
  const summary = detailTable([
    ['State', badge(node.state)],
    ['Reconciliation', badge(node.reconciliation)],
    ['Last seen', formatDate(node.last_seen_at)],
    ['Status age', node.last_seen_at ? `${esc(node.status_age_seconds)} seconds` : '<span class="muted">—</span>'],
    ['Host IP', display(node.host_ip)],
    ['Agent version', display(node.agent_version)],
    ['Services', `${node.running_services}/${node.desired_services} running`],
    ['vCPU', capacity(node.allocated.vcpus, node.capacity.vcpus, node.available.vcpus)],
    ['Memory', capacity(node.allocated.memory_mb, node.capacity.memory_mb, node.available.memory_mb, ' MB')],
    ['Registered', formatDate(node.registered_at)],
    ['Updated', formatDate(node.updated_at)],
    ['Status missing', formatBoolean(node.status_missing)],
    ['Status stale', formatBoolean(node.status_stale)],
    ['Reason', display(node.reason_code)],
    ['Message', display(node.message)],
  ]);

  const revisions = detailTable([
    ['Desired', display(node.desired_revision)],
    ['Placement', display(node.placement_revision)],
    ['Observed', display(node.observed_revision)],
    ['Applied', display(node.applied_revision)],
  ]);

  const serviceRows = (node.services || []).map(service => `<tr>
    <td>${serviceLink(service.name)}</td>
    <td>${badge(service.state)}</td>
    <td>${badge(service.health)}</td>
    <td>${service.vcpus}</td>
    <td>${service.memory_mb} MB</td>
  </tr>`);

  const conditionRows = (node.conditions || []).map(condition => `<tr>
    <td>${esc(condition.type)}</td>
    <td>${badge(condition.status)}</td>
    <td>${display(condition.reason_code)}</td>
    <td>${display(condition.message)}</td>
    <td>${formatDate(condition.last_transition_at)}</td>
  </tr>`);

  content.innerHTML = `<a class="back-link" href="#nodes">← Back to nodes</a>
    <div class="detail-heading"><div><p class="eyebrow">Node</p><h1>${esc(node.node_id)}</h1></div>${badge(node.state)}</div>
    <div class="detail-grid">
      ${section('Overview', summary)}
      ${section('Revisions', revisions)}
    </div>
    ${section('Services', table(['Service', 'State', 'Health', 'vCPU', 'Memory'], serviceRows, 'No services are assigned to this node.'))}
    ${(node.conditions || []).length ? section('Conditions', table(['Condition', 'Status', 'Reason', 'Message', 'Last transition'], conditionRows, '')) : ''}`;
}

function renderServiceDetail(service) {
  const summary = detailTable([
    ['State', badge(service.state)],
    ['Health', badge(service.health)],
    ['Desired node', nodeLink(service.desired_node)],
    ['Actual node', nodeLink(service.actual_node)],
    ['vCPU', display(service.vcpus)],
    ['Memory', `${esc(service.memory_mb)} MB`],
    ['PID', display(service.pid)],
    ['Restart count', display(service.restart_count)],
    ['Network address', display(service.network_address)],
    ['Routing hostname', display(service.routing_hostname)],
    ['Desired image', display(service.desired_image)],
    ['Desired kernel', display(service.desired_kernel)],
    ['Observed', formatDate(service.service_observed_at)],
    ['Last transition', formatDate(service.last_transition_at)],
    ['Reason', display(service.reason_code)],
    ['Message', display(service.message)],
  ]);

  const health = service.health_check || {};
  const healthCheck = detailTable([
    ['Type', display(health.type)],
    ['State', badge(health.state)],
    ['Last checked', formatDate(health.last_checked_at)],
    ['Failures', display(health.failures)],
    ['Last error', display(health.last_error)],
  ]);

  const revisions = detailTable([
    ['Desired', display(service.desired_revision)],
    ['Placement', display(service.placement_revision)],
    ['Rendered', display(service.rendered_revision)],
    ['Applied', display(service.applied_revision)],
  ]);

  const portRows = (service.port_forwards || []).map(port => `<tr>
    <td>${esc(port.HostPort ?? port.host_port)}</td>
    <td>${esc(port.VMPort ?? port.vm_port)}</td>
  </tr>`);

  content.innerHTML = `<a class="back-link" href="#services">← Back to services</a>
    <div class="detail-heading"><div><p class="eyebrow">Service</p><h1>${esc(service.name)}</h1></div>${badge(service.state)}</div>
    <div class="detail-grid">
      ${section('Overview', summary)}
      ${section('Health check', healthCheck)}
      ${section('Revisions', revisions)}
    </div>
    ${(service.port_forwards || []).length ? section('Port forwards', table(['Host port', 'VM port'], portRows, '')) : ''}`;
}

async function detail(kind, id) {
  const value = await api(`/v1/${kind}/${encodeURIComponent(decodeURIComponent(id))}`);
  updated.textContent = `Updated ${new Date(value.observed_at).toLocaleTimeString()}`;
  if (kind === 'nodes') {
    renderNodeDetail(value);
  } else {
    renderServiceDetail(value);
  }
}

async function load() {
  errorBox.hidden = true;
  try {
    const hash = location.hash.slice(1);
    if (hash.startsWith('node/')) {
      setActiveView('nodes');
      toolbar.hidden = true;
      return await detail('nodes', hash.slice(5));
    }
    if (hash.startsWith('service/')) {
      setActiveView('services');
      toolbar.hidden = true;
      return await detail('services', hash.slice(8));
    }
    if (hash === 'nodes' || hash === 'services') {
      setActiveView(hash);
      setFilters();
      const result = await api(`/v1/${view}`);
      data = result.items;
      updated.textContent = `Updated ${new Date(result.observed_at).toLocaleTimeString()}`;
      renderList();
      return;
    }

    setActiveView('overview');
    const [nodes, services] = await Promise.all([api('/v1/nodes'), api('/v1/services')]);
    const observedAt = new Date(Math.max(new Date(nodes.observed_at), new Date(services.observed_at)));
    updated.textContent = `Updated ${observedAt.toLocaleTimeString()}`;
    renderOverview(nodes.items, services.items);
  } catch (error) {
    errorBox.textContent = error.message;
    errorBox.hidden = false;
  }
}

navButtons.forEach(button => {
  button.addEventListener('click', () => {
    requestedFilter = '';
    filter.value = '';
    const hash = button.dataset.view === 'overview' ? '' : button.dataset.view;
    if (location.hash.slice(1) === hash) {
      load();
    } else {
      location.hash = hash;
    }
  });
});
search.addEventListener('input', renderList);
filter.addEventListener('change', renderList);
window.addEventListener('hashchange', load);
load();
setInterval(load, 7500);
