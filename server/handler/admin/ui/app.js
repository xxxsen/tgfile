const state = {
  session: null,
  csrf: "",
  path: "/",
  entriesCursor: "",
  jobsCursor: "",
  activeRequest: null,
  pollStarted: 0,
  pollTimer: 0,
};

const $ = (id) => document.getElementById(id);
const loginView = $("login-view");
const appView = $("app-view");
const filesView = $("files-view");
const backupView = $("backup-view");
const statusBox = $("status");

function showStatus(message) {
  statusBox.textContent = message;
  statusBox.classList.add("visible");
  window.setTimeout(() => statusBox.classList.remove("visible"), 3600);
}

async function api(url, options = {}) {
  const response = await fetch(url, {credentials: "same-origin", ...options});
  if (response.status === 401) {
    showLogin();
  }
  const payload = response.status === 204 ? null : await response.json().catch(() => null);
  if (!response.ok) {
    const error = new Error(payload?.error?.message || `请求失败 (${response.status})`);
    error.code = payload?.error?.code || "request_failed";
    error.status = response.status;
    throw error;
  }
  return payload?.data;
}

function mutationHeaders(extra = {}) {
  return {"X-CSRF-Token": state.csrf, ...extra};
}

function showLogin() {
  state.session = null;
  state.csrf = "";
  window.clearTimeout(state.pollTimer);
  appView.hidden = true;
  loginView.hidden = false;
  $("password").value = "";
}

function showApp(session) {
  state.session = session;
  state.csrf = session.csrf_token;
  loginView.hidden = true;
  appView.hidden = false;
  $("session-user").textContent = session.username;
  $("session-role").textContent = session.role;
  const writable = session.role === "read-write";
  $("upload-label").hidden = !writable;
  $("import-panel").hidden = !writable;
  void loadEntries(true);
}

async function restoreSession() {
  try {
    showApp(await api("/_admin/api/v1/session"));
  } catch {
    showLogin();
  }
}

$("login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const session = await api("/_admin/api/v1/session", {
      method: "POST",
      headers: {"Content-Type": "application/json"},
      body: JSON.stringify({username: $("username").value, password: $("password").value}),
    });
    $("password").value = "";
    showApp(session);
  } catch (error) {
    showStatus(error.message);
  }
});

$("logout-button").addEventListener("click", async () => {
  try {
    await api("/_admin/api/v1/session", {method: "DELETE", headers: mutationHeaders()});
  } catch (error) {
    showStatus(error.message);
  } finally {
    showLogin();
  }
});

function switchTab(tab) {
  const files = tab === "files";
  filesView.hidden = !files;
  backupView.hidden = files;
  $("files-tab").classList.toggle("active", files);
  $("backup-tab").classList.toggle("active", !files);
  $("files-tab").setAttribute("aria-selected", String(files));
  $("backup-tab").setAttribute("aria-selected", String(!files));
  if (!files) {
    $("export-scope").value = state.path;
    void loadJobs(true);
  } else {
    window.clearTimeout(state.pollTimer);
  }
}

$("files-tab").addEventListener("click", () => switchTab("files"));
$("backup-tab").addEventListener("click", () => switchTab("backup"));
$("refresh-files").addEventListener("click", () => void loadEntries(true));
$("load-more-files").addEventListener("click", () => void loadEntries(false));
$("refresh-jobs").addEventListener("click", () => void loadJobs(true));
$("load-more-jobs").addEventListener("click", () => void loadJobs(false));

function renderBreadcrumbs() {
  const container = $("breadcrumbs");
  container.replaceChildren();
  const root = document.createElement("button");
  root.type = "button";
  root.textContent = "/";
  root.addEventListener("click", () => navigate("/"));
  container.append(root);
  let current = "";
  for (const segment of state.path.split("/").filter(Boolean)) {
    const separator = document.createElement("span");
    separator.textContent = "›";
    container.append(separator);
    current += `/${segment}`;
    const target = current;
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = segment;
    button.addEventListener("click", () => navigate(target));
    container.append(button);
  }
}

function navigate(path) {
  state.path = path;
  void loadEntries(true);
}

async function loadEntries(reset) {
  if (reset) {
    state.entriesCursor = "";
    $("entries-body").replaceChildren();
  }
  const query = new URLSearchParams({path: state.path, limit: "100"});
  if (state.entriesCursor) query.set("cursor", state.entriesCursor);
  try {
    const data = await api(`/_admin/api/v1/entries?${query}`);
    renderBreadcrumbs();
    for (const item of data.items) renderEntry(item);
    state.entriesCursor = data.next_cursor || "";
    $("load-more-files").hidden = !state.entriesCursor;
  } catch (error) {
    if (error.code === "cursor_stale" && !reset) return loadEntries(true);
    showStatus(error.message);
  }
}

function renderEntry(item) {
  const row = document.createElement("tr");
  const nameCell = document.createElement("td");
  nameCell.dataset.label = "名称";
  if (item.kind === "directory") {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "name-button";
    button.textContent = item.name;
    button.addEventListener("click", () => navigate(item.path));
    nameCell.append(button);
  } else {
    nameCell.textContent = item.name;
  }
  const sizeCell = cell(item.kind === "directory" ? "—" : formatBytes(item.size), "大小");
  if (item.kind === "file") sizeCell.title = `${item.size} bytes`;
  row.append(nameCell, cell(item.kind === "directory" ? "目录" : "文件", "类型"),
    sizeCell, cell(formatTime(item.mtime), "修改时间"));
  const actions = document.createElement("td");
  actions.dataset.label = "操作";
  if (item.kind === "file") {
    const link = document.createElement("a");
    link.href = `/_admin/api/v1/content?${new URLSearchParams({path: item.path})}`;
    link.textContent = "下载";
    actions.append(link);
  }
  row.append(actions);
  $("entries-body").append(row);
}

function cell(value, label = "") {
  const element = document.createElement("td");
  element.textContent = value;
  if (label) element.dataset.label = label;
  return element;
}

function formatBytes(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let value = bytes;
  let index = -1;
  do { value /= 1024; index++; } while (value >= 1024 && index < units.length - 1);
  return `${value.toFixed(value >= 10 ? 1 : 2)} ${units[index]}`;
}

function formatTime(value) {
  return value ? new Date(value).toLocaleString() : "—";
}

$("upload-input").addEventListener("change", async (event) => {
  for (const file of [...event.target.files]) {
    if (!await uploadFile(file)) break;
  }
  event.target.value = "";
  await loadEntries(true);
});

async function uploadFile(file) {
  const target = state.path === "/" ? `/${file.name}` : `${state.path}/${file.name}`;
  let existing = null;
  try {
    existing = await api(`/_admin/api/v1/entries/stat?${new URLSearchParams({path: target})}`);
  } catch (error) {
    if (error.status !== 404) {
      showStatus(error.message);
      return false;
    }
  }
  const headers = {};
  if (existing) {
    if (existing.kind === "directory") {
      showStatus(`${file.name} 已存在且是目录`);
      return false;
    }
    if (!window.confirm(`覆盖 ${target}（${formatBytes(existing.size)}）？`)) return false;
    headers["If-Match"] = existing.etag;
  } else {
    headers["If-None-Match"] = "*";
  }
  try {
    await uploadXHR(`/_admin/api/v1/content?${new URLSearchParams({path: target})}`, file,
      mutationHeaders(headers), `上传 ${file.name}`);
    showStatus(`${file.name} 上传完成`);
    return true;
  } catch (error) {
    showStatus(error.message);
    return false;
  }
}

function uploadXHR(url, file, headers, title) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    state.activeRequest = xhr;
    showTransfer(title);
    xhr.open("PUT", url);
    xhr.withCredentials = true;
    for (const [name, value] of Object.entries(headers)) xhr.setRequestHeader(name, value);
    xhr.upload.onprogress = (event) => updateProgress(event.loaded, event.total);
    xhr.onload = () => {
      hideTransfer();
      const payload = parsePayload(xhr.responseText);
      if (xhr.status >= 200 && xhr.status < 300) resolve(payload?.data);
      else reject(apiError(xhr.status, payload));
    };
    xhr.onerror = () => { hideTransfer(); reject(new Error("网络连接失败")); };
    xhr.onabort = () => { hideTransfer(); reject(new Error("上传已取消")); };
    xhr.send(file);
  });
}

function importXHR(url, file, headers, title) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    state.activeRequest = xhr;
    showTransfer(title);
    xhr.open("POST", url);
    xhr.withCredentials = true;
    for (const [name, value] of Object.entries(headers)) xhr.setRequestHeader(name, value);
    xhr.upload.onprogress = (event) => updateProgress(event.loaded, event.total);
    xhr.onload = () => {
      hideTransfer();
      const payload = parsePayload(xhr.responseText);
      if (xhr.status >= 200 && xhr.status < 300) resolve(payload?.data);
      else reject(apiError(xhr.status, payload));
    };
    xhr.onerror = () => { hideTransfer(); reject(new Error("网络连接失败")); };
    xhr.onabort = () => { hideTransfer(); reject(new Error("上传已取消")); };
    xhr.send(file);
  });
}

function parsePayload(value) {
  try { return JSON.parse(value); } catch { return null; }
}

function apiError(status, payload) {
  const error = new Error(payload?.error?.message || `请求失败 (${status})`);
  error.code = payload?.error?.code || "request_failed";
  error.status = status;
  if (status === 401) showLogin();
  return error;
}

function showTransfer(title) {
  $("transfer").hidden = false;
  $("transfer-title").textContent = title;
  updateProgress(0, 0);
}

function updateProgress(loaded, total) {
  const percent = total ? Math.round(loaded * 100 / total) : 0;
  $("transfer-progress").value = percent;
  $("transfer-percent").textContent = total ? `${percent}%` : formatBytes(loaded);
}

function hideTransfer() {
  state.activeRequest = null;
  $("transfer").hidden = true;
}

$("cancel-transfer").addEventListener("click", () => state.activeRequest?.abort());
window.addEventListener("beforeunload", (event) => {
  if (state.activeRequest) {
    event.preventDefault();
    event.returnValue = "";
  }
});

$("export-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    await api("/_admin/api/v1/backup/exports", {
      method: "POST",
      headers: mutationHeaders({"Content-Type": "application/json", "Idempotency-Key": crypto.randomUUID()}),
      body: JSON.stringify({scope: $("export-scope").value}),
    });
    showStatus("导出任务已创建");
    await loadJobs(true);
  } catch (error) {
    showStatus(error.message);
  }
});

$("import-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const file = $("import-file").files[0];
  if (!file) return;
  const conflict = $("import-conflict").value;
  const dryRun = $("import-dry-run").checked;
  if (conflict === "replace" && !dryRun && window.prompt("输入 REPLACE 确认覆盖导入") !== "REPLACE") return;
  const query = new URLSearchParams({conflict, dry_run: String(dryRun)});
  const headers = mutationHeaders({
    "Content-Type": "application/vnd.tgfile.backup.v2+tar+gzip",
    "Idempotency-Key": crypto.randomUUID(),
  });
  if (conflict === "replace" && !dryRun) headers["X-Tgfile-Confirm-Replace"] = "true";
  try {
    await importXHR(`/_admin/api/v1/backup/imports?${query}`, file, headers, `导入 ${file.name}`);
    showStatus("导入任务已创建");
    await loadJobs(true);
  } catch (error) {
    showStatus(error.message);
  }
});

async function loadJobs(reset, polling = false) {
  if (reset) {
    state.jobsCursor = "";
    $("jobs-body").replaceChildren();
    if (!polling) state.pollStarted = Date.now();
  }
  const query = new URLSearchParams({limit: "50"});
  if (state.jobsCursor) query.set("cursor", state.jobsCursor);
  try {
    const data = await api(`/_admin/api/v1/backup/jobs?${query}`);
    for (const job of data.jobs) renderJob(job);
    state.jobsCursor = data.next_cursor || "";
    $("load-more-jobs").hidden = !state.jobsCursor;
    scheduleJobPoll(data.jobs);
  } catch (error) {
    showStatus(error.message);
  }
}

function renderJob(job) {
  const row = document.createElement("tr");
  row.append(cell(job.kind === "export" ? "导出" : job.dry_run ? "导入校验" : "导入", "类型"),
    cell(job.owner || "自己", "用户"), cell(job.state, "状态"),
    cell(jobProgress(job), "进度"), cell(formatTime(job.created_at), "创建时间"));
  const actions = document.createElement("td");
  actions.dataset.label = "操作";
  if (job.artifact_available) {
    const link = document.createElement("a");
    link.href = `/_admin/api/v1/backup/exports/${encodeURIComponent(job.job_id)}/artifact`;
    link.textContent = "下载";
    actions.append(link);
  }
  if (job.cancelable) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "secondary";
    button.textContent = "取消";
    button.addEventListener("click", () => void cancelJob(job.job_id));
    actions.append(button);
  }
  if (job.error?.message) actions.title = `${job.error.code}: ${job.error.message}`;
  row.append(actions);
  $("jobs-body").append(row);
}

function jobProgress(job) {
  const done = job.progress.bytes_completed;
  const total = job.progress.bytes_total;
  if (total > 0) return `${Math.round(done * 100 / total)}%`;
  return `${job.progress.files_completed}/${job.progress.files_total}`;
}

async function cancelJob(jobID) {
  try {
    await api(`/_admin/api/v1/backup/jobs/${encodeURIComponent(jobID)}/cancel`,
      {method: "POST", headers: mutationHeaders()});
    showStatus("已请求取消任务");
    await loadJobs(true);
  } catch (error) {
    showStatus(error.message);
  }
}

function scheduleJobPoll(jobs) {
  window.clearTimeout(state.pollTimer);
  const active = jobs.some((job) => !["succeeded", "failed", "canceled"].includes(job.state));
  if (!active || document.hidden || backupView.hidden) return;
  const delay = Date.now() - state.pollStarted < 30000 ? 1000 : 5000;
  state.pollTimer = window.setTimeout(() => void loadJobs(true, true), delay);
}

document.addEventListener("visibilitychange", () => {
  if (!document.hidden && !backupView.hidden) void loadJobs(true);
});

void restoreSession();
