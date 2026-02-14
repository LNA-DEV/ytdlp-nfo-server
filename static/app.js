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

const jobs = new Map();
const eventSources = new Map();

// --- Tabs ---

tabBtns.forEach(btn => {
  btn.addEventListener('click', () => {
    tabBtns.forEach(b => b.classList.remove('active'));
    tabPanels.forEach(p => p.classList.remove('active'));
    btn.classList.add('active');
    document.getElementById(btn.dataset.panel).classList.add('active');
  });
});

function updateTabCounts() {
  let active = 0, failed = 0;
  for (const job of jobs.values()) {
    if (job.status === 'failed') failed++;
    else active++;
  }
  activeCount.textContent = active || '';
  failedCount.textContent = failed || '';
  activeEmpty.style.display = active === 0 ? '' : 'none';
  failedEmpty.style.display = failed === 0 ? '' : 'none';
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
      alert(err.error || 'Failed to start download');
      return;
    }
    const job = await resp.json();
    addJobCard(job);
    streamJob(job.id);
    urlInput.value = '';
  } finally {
    dlBtn.disabled = false;
  }
  urlInput.focus();
}

// --- Job Cards ---

function addJobCard(job) {
  if (jobs.has(job.id)) {
    // Update existing entry
    jobs.set(job.id, { ...jobs.get(job.id), ...job });
    placeCard(job.id);
    updateBadge(job.id, job.status);
    updateTabCounts();
    return;
  }
  jobs.set(job.id, job);

  const card = document.createElement('div');
  card.className = 'job-card';
  card.id = 'job-' + job.id;

  const header = document.createElement('div');
  header.className = 'job-header';
  header.onclick = () => {
    card.querySelector('.job-output').classList.toggle('open');
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

  header.appendChild(badge);
  header.appendChild(urlSpan);
  header.appendChild(timeSpan);
  header.appendChild(retryBtn);
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
  output.className = 'job-output' + (job.status === 'pending' || job.status === 'running' || job.status === 'retrying' ? ' open' : '');

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
  updateTabCounts();
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

  es.onmessage = (e) => {
    if (pre) {
      pre.textContent += e.data + '\n';
      pre.parentElement.scrollTop = pre.parentElement.scrollHeight;
    }
  };

  es.addEventListener('progress', (e) => {
    const pct = parseFloat(e.data);
    const bar = document.getElementById('progress-' + id);
    if (bar && !isNaN(pct)) {
      bar.style.width = pct + '%';
    }
  });

  es.addEventListener('status', (e) => {
    const status = e.data;
    const job = jobs.get(id);
    if (job) job.status = status;
    updateBadge(id, status);
    placeCard(id);
    updateTabCounts();

    const retryBtn = document.getElementById('retry-' + id);
    if (retryBtn) {
      retryBtn.style.display = status === 'failed' ? '' : 'none';
    }
  });

  es.addEventListener('done', (e) => {
    const status = e.data;
    const job = jobs.get(id);
    if (job) job.status = status;
    updateBadge(id, status);
    placeCard(id);
    updateTabCounts();

    const retryBtn = document.getElementById('retry-' + id);
    if (retryBtn) {
      retryBtn.style.display = status === 'failed' ? '' : 'none';
    }

    // Set progress bar to 100% on completion
    if (status === 'completed') {
      const bar = document.getElementById('progress-' + id);
      if (bar) bar.style.width = '100%';
    }

    es.close();
    eventSources.delete(id);
  });

  es.onerror = () => {
    es.close();
    eventSources.delete(id);
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
      alert(err.error || 'Failed to retry');
      if (retryBtn) retryBtn.disabled = false;
      return;
    }

    const job = await resp.json();
    jobs.set(id, { ...jobs.get(id), ...job });

    // Clear output and error
    const pre = document.getElementById('output-' + id);
    if (pre) pre.textContent = '';
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
    updateTabCounts();

    // Reconnect SSE
    streamJob(id);
  } catch (e) {
    if (retryBtn) retryBtn.disabled = false;
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
      if (list[i].status === 'pending' || list[i].status === 'running' || list[i].status === 'retrying') {
        streamJob(list[i].id);
      } else {
        loadJobOutput(list[i].id);
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
    if (pre && job.output) {
      pre.textContent = job.output.join('\n') + '\n';
    }
    // Collapse completed/failed jobs by default on page load
    const card = document.getElementById('job-' + id);
    if (card) {
      card.querySelector('.job-output').classList.remove('open');
    }
  } catch (e) {
    // ignore
  }
}

loadJobs();
