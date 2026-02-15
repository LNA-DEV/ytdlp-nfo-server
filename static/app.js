const activeList = document.getElementById('active-jobs');
const failedList = document.getElementById('failed-jobs');
const activeEmpty = document.getElementById('active-empty');
const failedEmpty = document.getElementById('failed-empty');
const urlInput = document.getElementById('url-input');
const dlBtn = document.getElementById('dl-btn');
const tabBtns = document.querySelectorAll('.tab');
const tabPanels = document.querySelectorAll('.tab-panel');
const activeCount = document.getElementById('active-count');
const failedCount = document.getElementById('failed-count');

const modalOverlay = document.getElementById('modal-overlay');
const modalMessage = document.getElementById('modal-message');
const modalActions = document.getElementById('modal-actions');

const jobs = new Map();
const eventSources = new Map();
const jobLines = new Map();
const pendingOutputUpdates = new Set();
const pendingProgress = new Map();
let tabActive = 0, tabFailed = 0;

// --- Modal helpers ---

function showAlert(message) {
  return new Promise((resolve) => {
    modalMessage.textContent = message;
    modalActions.innerHTML = '';

    const okBtn = document.createElement('button');
    okBtn.className = 'modal-btn modal-btn-primary';
    okBtn.textContent = 'OK';

    function close() {
      modalOverlay.classList.remove('open');
      modalOverlay.removeEventListener('click', onOverlay);
      resolve();
    }

    function onOverlay(e) {
      if (e.target === modalOverlay) close();
    }

    okBtn.onclick = close;
    modalOverlay.addEventListener('click', onOverlay);
    modalActions.appendChild(okBtn);
    modalOverlay.classList.add('open');
  });
}

function showConfirm(message) {
  return new Promise((resolve) => {
    modalMessage.textContent = message;
    modalActions.innerHTML = '';

    const cancelBtn = document.createElement('button');
    cancelBtn.className = 'modal-btn modal-btn-cancel';
    cancelBtn.textContent = 'Cancel';

    const deleteBtn = document.createElement('button');
    deleteBtn.className = 'modal-btn modal-btn-danger';
    deleteBtn.textContent = 'Delete';

    function close(result) {
      modalOverlay.classList.remove('open');
      modalOverlay.removeEventListener('click', onOverlay);
      resolve(result);
    }

    function onOverlay(e) {
      if (e.target === modalOverlay) close(false);
    }

    cancelBtn.onclick = () => close(false);
    deleteBtn.onclick = () => close(true);
    modalOverlay.addEventListener('click', onOverlay);
    modalActions.appendChild(cancelBtn);
    modalActions.appendChild(deleteBtn);
    modalOverlay.classList.add('open');
  });
}

// --- Tabs ---

tabBtns.forEach(btn => {
  btn.addEventListener('click', () => {
    tabBtns.forEach(b => b.classList.remove('active'));
    tabPanels.forEach(p => p.classList.remove('active'));
    btn.classList.add('active');
    document.getElementById(btn.dataset.panel).classList.add('active');
  });
});

function renderTabCounts() {
  activeCount.textContent = tabActive || '';
  failedCount.textContent = tabFailed || '';
  activeEmpty.style.display = tabActive === 0 ? '' : 'none';
  failedEmpty.style.display = tabFailed === 0 ? '' : 'none';
}

function adjustTabCounts(oldStatus, newStatus) {
  if (oldStatus) {
    if (oldStatus === 'failed') tabFailed--;
    else tabActive--;
  }
  if (newStatus) {
    if (newStatus === 'failed') tabFailed++;
    else tabActive++;
  }
  renderTabCounts();
}

// --- Submit ---

urlInput.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') submitDownload();
});

async function submitDownload() {
  const url = urlInput.value.trim();
  if (!url) return;

  dlBtn.disabled = true;
  try {
    const resp = await fetch('/api/download', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({url})
    });
    if (!resp.ok) {
      const err = await resp.json();
      showAlert(err.error || 'Failed to start download');
      return;
    }
    const job = await resp.json();
    addJobCard(job);
    if (job.status !== 'queued') {
      streamJob(job.id);
    }
    urlInput.value = '';
  } finally {
    dlBtn.disabled = false;
  }
  urlInput.focus();
}

// --- Job Cards ---

function addJobCard(job) {
  if (jobs.has(job.id)) {
    const oldStatus = jobs.get(job.id).status;
    jobs.set(job.id, { ...jobs.get(job.id), ...job });
    placeCard(job.id);
    updateBadge(job.id, job.status);
    if (oldStatus !== job.status) adjustTabCounts(oldStatus, job.status);
    return;
  }
  jobs.set(job.id, job);

  const card = document.createElement('div');
  card.className = 'job-card';
  card.id = 'job-' + job.id;

  const header = document.createElement('div');
  header.className = 'job-header';
  header.onclick = () => {
    const outputPanel = card.querySelector('.job-output');
    const isOpening = !outputPanel.classList.contains('open');
    outputPanel.classList.toggle('open');
    if (isOpening) {
      const pre = document.getElementById('output-' + job.id);
      if (pre && !pre.firstChild && !eventSources.has(job.id)) {
        loadJobOutput(job.id);
      }
    }
  };

  const badge = document.createElement('span');
  badge.className = 'badge badge-' + job.status;
  badge.id = 'badge-' + job.id;
  badge.textContent = job.status;

  const urlSpan = document.createElement('span');
  urlSpan.className = 'job-url';
  urlSpan.textContent = job.url;

  const timeSpan = document.createElement('span');
  timeSpan.className = 'job-time';
  timeSpan.textContent = formatTime(job.createdAt);

  const retryBtn = document.createElement('button');
  retryBtn.className = 'retry-btn';
  retryBtn.id = 'retry-' + job.id;
  retryBtn.textContent = 'Retry';
  retryBtn.style.display = job.status === 'failed' ? '' : 'none';
  retryBtn.onclick = (e) => { e.stopPropagation(); retryJob(job.id); };

  const deleteBtn = document.createElement('button');
  deleteBtn.className = 'delete-btn';
  deleteBtn.title = 'Delete job';
  deleteBtn.innerHTML = '&#x2715;';
  deleteBtn.onclick = (e) => { e.stopPropagation(); deleteJob(job.id); };

  header.appendChild(badge);
  header.appendChild(urlSpan);
  header.appendChild(timeSpan);
  header.appendChild(retryBtn);
  header.appendChild(deleteBtn);
  card.appendChild(header);

  // Progress bar
  const progressContainer = document.createElement('div');
  progressContainer.className = 'progress-bar-container';
  progressContainer.id = 'progress-container-' + job.id;
  const progressBar = document.createElement('div');
  progressBar.className = 'progress-bar';
  progressBar.id = 'progress-' + job.id;
  if (job.progress > 0) progressBar.style.width = job.progress + '%';
  progressContainer.appendChild(progressBar);
  card.appendChild(progressContainer);

  const output = document.createElement('div');
  output.className = 'job-output' + (job.status === 'pending' || job.status === 'running' || job.status === 'retrying' || job.status === 'queued' ? ' open' : '');

  const pre = document.createElement('pre');
  pre.id = 'output-' + job.id;
  output.appendChild(pre);
  card.appendChild(output);

  if (job.error) {
    const errDiv = document.createElement('div');
    errDiv.className = 'job-error';
    errDiv.id = 'error-' + job.id;
    errDiv.textContent = job.error;
    card.appendChild(errDiv);
  }

  placeCard(job.id, card);
  adjustTabCounts(null, job.status);
}

function placeCard(id, card) {
  card = card || document.getElementById('job-' + id);
  if (!card) return;
  const job = jobs.get(id);
  if (!job) return;

  if (job.status === 'failed') {
    if (card.parentElement !== failedList) {
      card.remove();
      failedList.prepend(card);
    }
  } else {
    if (card.parentElement !== activeList) {
      card.remove();
      activeList.prepend(card);
    }
  }
}

// --- SSE Streaming ---

function streamJob(id) {
  // Close existing stream if any
  if (eventSources.has(id)) {
    eventSources.get(id).close();
    eventSources.delete(id);
  }

  const es = new EventSource('/api/jobs/' + id + '/stream');
  eventSources.set(id, es);
  const pre = document.getElementById('output-' + id);

  jobLines.set(id, []);

  es.onmessage = (e) => {
    if (pre) {
      let batch = jobLines.get(id);
      if (!batch) { batch = []; jobLines.set(id, batch); }
      batch.push(e.data);
      if (!pendingOutputUpdates.has(id)) {
        pendingOutputUpdates.add(id);
        requestAnimationFrame(() => {
          pendingOutputUpdates.delete(id);
          const b = jobLines.get(id);
          if (b && b.length && pre) {
            pre.appendChild(document.createTextNode(b.join('\n') + '\n'));
            b.length = 0;
            pre.parentElement.scrollTop = pre.parentElement.scrollHeight;
          }
        });
      }
    }
  };

  es.addEventListener('progress', (e) => {
    const pct = parseFloat(e.data);
    if (!isNaN(pct)) {
      const hadPending = pendingProgress.has(id);
      pendingProgress.set(id, pct);
      if (!hadPending) {
        requestAnimationFrame(() => {
          const p = pendingProgress.get(id);
          pendingProgress.delete(id);
          const bar = document.getElementById('progress-' + id);
          if (bar && p !== undefined) bar.style.width = p + '%';
        });
      }
    }
  });

  es.addEventListener('status', (e) => {
    const status = e.data;
    const job = jobs.get(id);
    const oldStatus = job ? job.status : null;
    if (job) job.status = status;
    updateBadge(id, status);
    placeCard(id);
    if (oldStatus !== status) adjustTabCounts(oldStatus, status);

    const retryBtn = document.getElementById('retry-' + id);
    if (retryBtn) {
      retryBtn.style.display = status === 'failed' ? '' : 'none';
    }
  });

  es.addEventListener('done', (e) => {
    const status = e.data;
    const job = jobs.get(id);
    const oldStatus = job ? job.status : null;
    if (job) job.status = status;
    updateBadge(id, status);
    placeCard(id);
    if (oldStatus !== status) adjustTabCounts(oldStatus, status);

    const retryBtn = document.getElementById('retry-' + id);
    if (retryBtn) {
      retryBtn.style.display = status === 'failed' ? '' : 'none';
    }

    // Set progress bar to 100% on completion
    if (status === 'completed') {
      const bar = document.getElementById('progress-' + id);
      if (bar) bar.style.width = '100%';
    }

    jobLines.delete(id);
    pendingOutputUpdates.delete(id);
    pendingProgress.delete(id);
    es.close();
    eventSources.delete(id);
  });

  es.onerror = () => {
    const job = jobs.get(id);
    if (job && (job.status === 'completed' || job.status === 'failed')) {
      jobLines.delete(id);
      pendingOutputUpdates.delete(id);
      pendingProgress.delete(id);
      es.close();
      eventSources.delete(id);
    }
    // Otherwise let EventSource auto-retry
  };
}

// --- Badge ---

function updateBadge(id, status) {
  const badge = document.getElementById('badge-' + id);
  if (!badge) return;
  badge.className = 'badge badge-' + status;
  badge.textContent = status;
}

// --- Retry ---

async function retryJob(id) {
  const retryBtn = document.getElementById('retry-' + id);
  if (retryBtn) retryBtn.disabled = true;

  try {
    const resp = await fetch('/api/jobs/' + id + '/retry', { method: 'POST' });
    if (!resp.ok) {
      const err = await resp.json();
      showAlert(err.error || 'Failed to retry');
      if (retryBtn) retryBtn.disabled = false;
      return;
    }

    const oldStatus = (jobs.get(id) || {}).status;
    const job = await resp.json();
    jobs.set(id, { ...jobs.get(id), ...job });

    // Clear output and error
    const pre = document.getElementById('output-' + id);
    if (pre) pre.textContent = '';
    jobLines.delete(id);
    const errDiv = document.getElementById('error-' + id);
    if (errDiv) errDiv.remove();

    // Reset progress bar
    const bar = document.getElementById('progress-' + id);
    if (bar) bar.style.width = '0%';

    // Update UI
    updateBadge(id, job.status);
    if (retryBtn) retryBtn.style.display = 'none';

    // Open output panel
    const card = document.getElementById('job-' + id);
    if (card) card.querySelector('.job-output').classList.add('open');

    placeCard(id);
    if (oldStatus !== job.status) adjustTabCounts(oldStatus, job.status);

    // Reconnect SSE if not queued
    if (job.status !== 'queued') {
      streamJob(id);
    }
  } catch (e) {
    if (retryBtn) retryBtn.disabled = false;
  }
}

// --- Delete ---

async function deleteJob(id) {
  try {
    const resp = await fetch('/api/jobs/' + id, { method: 'DELETE' });
    if (!resp.ok) return;

    // Close SSE if active
    if (eventSources.has(id)) {
      eventSources.get(id).close();
      eventSources.delete(id);
    }
    jobLines.delete(id);
    pendingOutputUpdates.delete(id);
    pendingProgress.delete(id);

    // Remove card from DOM
    const card = document.getElementById('job-' + id);
    if (card) card.remove();

    const oldStatus = (jobs.get(id) || {}).status;
    jobs.delete(id);
    adjustTabCounts(oldStatus, null);
  } catch (e) {
    // ignore
  }
}

async function deleteAllJobs() {
  if (!(await showConfirm('Delete all jobs? This cannot be undone.'))) return;

  try {
    const resp = await fetch('/api/jobs', { method: 'DELETE' });
    if (!resp.ok) return;

    // Close all SSE connections
    for (const [id, es] of eventSources) {
      es.close();
    }
    eventSources.clear();
    jobLines.clear();
    pendingOutputUpdates.clear();
    pendingProgress.clear();

    // Remove all job cards
    for (const [id] of jobs) {
      const card = document.getElementById('job-' + id);
      if (card) card.remove();
    }
    jobs.clear();
    tabActive = 0;
    tabFailed = 0;
    renderTabCounts();
  } catch (e) {
    // ignore
  }
}

// --- Utilities ---

function formatTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  return d.toLocaleTimeString();
}

// --- Load existing jobs on page load ---

async function loadJobs() {
  try {
    const resp = await fetch('/api/jobs');
    if (!resp.ok) return;
    const list = await resp.json();
    // list is newest-first, reverse to prepend in correct order
    for (let i = list.length - 1; i >= 0; i--) {
      addJobCard(list[i]);
      if (list[i].status === 'pending' || list[i].status === 'running' || list[i].status === 'retrying' || list[i].status === 'queued') {
        streamJob(list[i].id);
      }
    }
  } catch (e) {
    // ignore
  }
}

async function loadJobOutput(id) {
  try {
    const resp = await fetch('/api/jobs/' + id);
    if (!resp.ok) return;
    const job = await resp.json();
    const pre = document.getElementById('output-' + id);
    if (pre && job.output && job.output.length) {
      pre.textContent = job.output.join('\n') + '\n';
    } else if (pre) {
      pre.textContent = '(output not available)\n';
    }
  } catch (e) {
    // ignore
  }
}

loadJobs();
