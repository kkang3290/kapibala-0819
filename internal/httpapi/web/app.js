const taskRows = document.querySelector('#task-rows');
const form = document.querySelector('#create-form');
const stepEditors = document.querySelector('#step-editors');
const createDialog = document.querySelector('#create-dialog');
const claimDialog = document.querySelector('#claim-dialog');
const reportDialog = document.querySelector('#report-dialog');
const toast = document.querySelector('#toast');
let toastTimer;
let pollTimer;
let isRefreshing = false;
const taskPageSize = 25;
let taskOffset = 0;

async function apiFetch(url, options = {}) {
  return fetch(url, options);
}

async function bootstrap() {
  await refresh();
  clearInterval(pollTimer);
  pollTimer = setInterval(refresh, 1000);
}

function addParameterRow(container, parameter = {}) {
  const row = document.createElement('div');
  row.className = 'parameter-row';
  row.innerHTML = `
    <input class="param-key" aria-label="参数名" placeholder="参数名" autocomplete="off">
    <select class="param-type" aria-label="值类型">
      <option value="string">文本</option>
      <option value="number">数字</option>
      <option value="boolean">布尔值</option>
      <option value="json">JSON</option>
    </select>
    <input class="param-value" aria-label="参数值" placeholder="参数值" autocomplete="off">
    <select class="param-boolean" aria-label="布尔值">
      <option value="true">true</option>
      <option value="false">false</option>
    </select>
    <button class="icon-button remove-param" type="button" aria-label="删除参数" title="删除参数">&times;</button>
  `;
  row.querySelector('.param-key').value = parameter.key || '';
  row.querySelector('.param-type').value = parameter.type || 'string';
  row.querySelector('.param-value').value = parameter.value ?? '';
  row.querySelector('.param-boolean').value = String(parameter.value ?? true);
  row.querySelector('.param-type').addEventListener('change', () => syncParameterType(row));
  row.querySelector('.remove-param').addEventListener('click', () => row.remove());
  container.append(row);
  syncParameterType(row);
}

function syncParameterType(row) {
  const isBoolean = row.querySelector('.param-type').value === 'boolean';
  row.querySelector('.param-value').hidden = isBoolean;
  row.querySelector('.param-boolean').hidden = !isBoolean;
}

function addStep(parameters = []) {
  const position = stepEditors.children.length + 1;
  const editor = document.createElement('div');
  editor.className = 'step-editor';
  editor.innerHTML = `
    <div class="step-editor-title">
      <strong>Step ${position}</strong>
      <button class="icon-button remove-step" type="button" aria-label="删除 Step ${position}" title="删除 Step ${position}">&times;</button>
    </div>
    <div class="parameter-list"></div>
    <button class="text-button add-step-param" type="button">添加步骤参数</button>
  `;
  editor.querySelector('.remove-step').addEventListener('click', () => {
    if (stepEditors.children.length === 1) return;
    editor.remove();
    renumberSteps();
  });
  const list = editor.querySelector('.parameter-list');
  editor.querySelector('.add-step-param').addEventListener('click', () => addParameterRow(list));
  parameters.forEach((parameter) => addParameterRow(list, parameter));
  stepEditors.append(editor);
  renumberSteps();
}

function renumberSteps() {
  const onlyStep = stepEditors.children.length === 1;
  [...stepEditors.children].forEach((editor, index) => {
    editor.querySelector('strong').textContent = `Step ${index + 1}`;
    editor.querySelector('.remove-step').setAttribute('aria-label', `删除 Step ${index + 1}`);
    editor.querySelector('.remove-step').setAttribute('title', `删除 Step ${index + 1}`);
    editor.querySelector('.remove-step').disabled = onlyStep;
  });
}

function serializeParameters(container, label) {
  const result = {};
  for (const row of container.querySelectorAll(':scope > .parameter-row')) {
    const key = row.querySelector('.param-key').value.trim();
    if (!key) throw new Error(`${label}存在空参数名`);
    if (Object.hasOwn(result, key)) throw new Error(`${label}的参数 ${key} 重复`);
    const type = row.querySelector('.param-type').value;
    const raw = type === 'boolean'
      ? row.querySelector('.param-boolean').value
      : row.querySelector('.param-value').value;
    switch (type) {
      case 'string':
        result[key] = raw;
        break;
      case 'number': {
        const number = Number(raw);
        if (raw.trim() === '' || !Number.isFinite(number)) throw new Error(`${label}的参数 ${key} 不是有效数字`);
        result[key] = number;
        break;
      }
      case 'boolean':
        result[key] = raw === 'true';
        break;
      case 'json':
        try {
          result[key] = JSON.parse(raw);
        } catch {
          throw new Error(`${label}的参数 ${key} 不是有效 JSON`);
        }
        break;
    }
  }
  return result;
}

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  const error = document.querySelector('#form-error');
  error.textContent = '';
  try {
    const body = {
      group_name: document.querySelector('#group-name').value.trim(),
      group_overrides: serializeParameters(document.querySelector('#group-parameters'), '组参数'),
      base_parameters: serializeParameters(document.querySelector('#base-parameters'), '默认参数'),
      steps: [...stepEditors.children].map((editor, index) => ({
        overrides: serializeParameters(editor.querySelector('.parameter-list'), `Step ${index + 1}`),
      })),
    };
    const response = await apiFetch('/api/tasks', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(body),
    });
    const payload = await response.json();
    if (!response.ok) throw new Error(payload.error || '创建失败');
    createDialog.close();
    showToast(`任务 #${payload.id} 已创建`);
    taskOffset = 0;
    await refresh();
  } catch (requestError) {
    error.textContent = requestError.message;
  }
});

async function refresh() {
  if (isRefreshing) return;
  isRefreshing = true;
  try {
    const [taskResponse, activityResponse] = await Promise.all([
      apiFetch(`/api/tasks?limit=${taskPageSize}&offset=${taskOffset}`, {cache: 'no-store'}),
      apiFetch('/api/activity?limit=60', {cache: 'no-store'}),
    ]);
    if (!taskResponse.ok || !activityResponse.ok) throw new Error('加载看板失败');
    const [taskPage, {logs}] = await Promise.all([taskResponse.json(), activityResponse.json()]);
    if (taskPage.summary.total > 0 && taskPage.offset >= taskPage.summary.total) {
      taskOffset = Math.floor((taskPage.summary.total - 1) / taskPageSize) * taskPageSize;
    }
    renderTasks(taskPage.tasks, taskPage.summary, taskPage.limit, taskPage.offset);
    renderActivity(logs);
    document.querySelector('#connection-state').textContent = '实时轮询中';
    document.querySelector('#connection-state').className = 'online';
    document.querySelector('#last-updated').textContent = new Date().toLocaleTimeString('zh-CN');
  } catch (error) {
    document.querySelector('#connection-state').textContent = error.message;
    document.querySelector('#connection-state').className = 'offline';
  } finally {
    isRefreshing = false;
  }
}

function renderActivity(logs) {
  const container = document.querySelector('#activity-log');
  if (!logs.length) {
    container.innerHTML = '<div class="activity-empty">暂无运行日志</div>';
    return;
  }
  container.innerHTML = logs.map((log) => {
    const content = activityContent(log);
    return `
      <div class="activity-entry event-${log.event_type}">
        <time>${formatLogTime(log.created_at)}</time>
        <div class="activity-marker"></div>
        <div class="activity-content">
          <strong>${escapeHTML(content.title)}</strong>
          <span>${escapeHTML(content.detail)}</span>
        </div>
      </div>
    `;
  }).join('');
}

function activityContent(log) {
  const step = log.step_position ? ` · Step ${log.step_position}` : '';
  switch (log.event_type) {
    case 'task_created':
      return {title: `Task #${log.task_id} 已创建`, detail: `${log.details.group_name} · ${log.details.step_count} Steps`};
    case 'task_claimed':
      if (log.details.claim_demo) {
        return log.details.won
          ? {title: `${log.worker_id} 认领 Task #${log.task_id} 成功`, detail: `唯一 owner · ${Number(log.details.duration_ms).toFixed(1)} ms`}
          : {title: `${log.worker_id} 未获得 Task #${log.task_id}`, detail: `已由 ${log.details.winner} 持有`};
      }
      return {title: `Task #${log.task_id} 已认领`, detail: `${log.worker_id || '--'} · token #${log.details.fencing_token}`};
    case 'task_reclaimed':
      return {title: `Task #${log.task_id} 失联后已接管`, detail: `${log.details.previous_worker || '--'} → ${log.worker_id} · token #${log.details.fencing_token}`};
    case 'step_started':
      return {
        title: `Task #${log.task_id}${step} ${log.details.resumed ? '恢复执行' : '开始执行'}`,
        detail: `${log.details.idempotency_key || '--'} · ${compactJSON(log.details.resolved_parameters)}`,
      };
    case 'step_reported':
      return log.details.duplicate_report
        ? {title: `Task #${log.task_id}${step} 重复上报已去重`, detail: `执行日志保持 ${log.details.log_count} 条`}
        : {title: `Task #${log.task_id}${step} 完成上报`, detail: log.details.final_success ? '首次写入 · 成功' : '首次写入 · 失败'};
    case 'task_done':
      return {title: `Task #${log.task_id} 全部完成`, detail: '状态更新为 done'};
    case 'task_failed':
      return {title: `Task #${log.task_id}${step} 执行失败`, detail: '状态更新为 failed'};
    case 'task_cancelled':
      return {title: `Task #${log.task_id} 已取消`, detail: `操作人 ${log.details.actor || '--'} · Worker 写入已阻止`};
    default:
      return {title: `Task #${log.task_id}`, detail: log.event_type};
  }
}

function formatLogTime(value) {
  return new Date(value).toLocaleTimeString('zh-CN', {hour12: false});
}

function renderTasks(tasks, summary, limit, offset) {
  document.querySelector('#metric-total').textContent = summary.total;
  document.querySelector('#metric-pending').textContent = summary.pending;
  document.querySelector('#metric-active').textContent = summary.active;
  document.querySelector('#metric-finished').textContent = summary.finished;
  const first = summary.total ? offset + 1 : 0;
  const last = Math.min(offset + tasks.length, summary.total);
  document.querySelector('#task-count').textContent = `${first}-${last} / ${summary.total} 条记录`;
  document.querySelector('#page-range').textContent = `${first}-${last} / ${summary.total}`;
  document.querySelector('#previous-page').disabled = offset === 0;
  document.querySelector('#next-page').disabled = offset + limit >= summary.total;

  if (!tasks.length) {
    taskRows.innerHTML = '<tr><td class="empty" colspan="7">暂无任务</td></tr>';
    return;
  }
  taskRows.innerHTML = tasks.map((task) => `
    <tr class="task-main-row">
      <td class="task-id">#${task.id}</td>
      <td>${escapeHTML(task.group_name)}</td>
      <td><span class="status status-${task.status}">${task.status}</span></td>
      <td class="owner-cell">
        <span class="mono">${escapeHTML(task.claimed_by || '--')}</span>
        ${task.claimed_by ? `<small>token #${task.fencing_token} · ${leaseLabel(task.lease_expires_at)}</small>` : ''}
      </td>
      <td>${task.current_step ? `${task.current_step} / ${task.steps.length}` : `-- / ${task.steps.length}`}</td>
      <td><code>${escapeHTML(compactJSON(task.effective_parameters))}</code></td>
      <td>${['pending', 'claimed', 'running'].includes(task.status) ? `<button class="text-button cancel-task" data-cancel="${task.id}" type="button">取消任务</button>` : '--'}</td>
    </tr>
    <tr class="task-detail-row">
      <td colspan="7">
        <div class="steps">${task.steps.map((step) => renderStep(task, step)).join('')}</div>
      </td>
    </tr>
  `).join('');

  document.querySelectorAll('[data-complete]').forEach((button) => {
    button.addEventListener('click', () => reportFive(button));
  });
  document.querySelectorAll('[data-cancel]').forEach((button) => {
    button.addEventListener('click', () => cancelTask(button));
  });
}

function renderStep(task, step) {
  const canComplete = task.status === 'running' && task.current_step === step.position && step.status === 'running';
  const result = step.log ? (step.log.success ? '成功' : '失败') : '--';
  return `
    <div class="step ${step.status === 'running' ? 'current' : ''}">
      <div class="step-title">
        <strong>Step ${step.position}</strong>
        <span class="status status-${step.status}">${step.status}</span>
      </div>
      <dl>
        <div><dt>Override</dt><dd><code>${escapeHTML(compactJSON(step.overrides))}</code></dd></div>
        <div><dt>解析参数</dt><dd><code>${escapeHTML(compactJSON(step.resolved_parameters))}</code></dd></div>
        <div><dt>执行日志</dt><dd>${result}</dd></div>
      </dl>
      ${canComplete ? `<button class="button danger" data-complete="${task.id}:${step.position}" data-worker="${escapeHTML(task.claimed_by)}" data-token="${task.fencing_token}">验证 5 路并发</button>` : ''}
    </div>
  `;
}

function leaseLabel(value) {
  if (!value) return '无租约';
  const remaining = new Date(value).getTime() - Date.now();
  if (remaining <= 0) return '租约已到期';
  return `租约 ${Math.ceil(remaining / 1000)}s`;
}

async function cancelTask(button) {
  const taskID = button.dataset.cancel;
  if (!window.confirm(`确定取消 Task #${taskID}？正在执行的 Worker 会被立即阻止继续写入。`)) return;
  button.disabled = true;
  try {
    const response = await apiFetch(`/api/tasks/${taskID}/cancel`, {method: 'POST'});
    const payload = await response.json();
    if (!response.ok) throw new Error(payload.error || '取消失败');
    showToast(`任务 #${taskID} 已取消`);
    await refresh();
  } catch (error) {
    showToast(error.message, true);
    button.disabled = false;
  }
}

async function runClaimDemo(button) {
  button.disabled = true;
  prepareClaimDialog();
  claimDialog.showModal();
  await new Promise((resolve) => setTimeout(resolve, 450));
  try {
    const response = await apiFetch('/api/demos/claim-race', {method: 'POST'});
    const payload = await response.json();
    if (!response.ok) throw new Error(payload.error || '并发认领验证失败');
    renderClaimResults(payload);
    await refresh();
  } catch (error) {
    const verdict = document.querySelector('#claim-verdict');
    verdict.className = 'report-verdict failed';
    verdict.innerHTML = `<span class="verdict-mark">!</span><div><strong>验证未通过</strong><span>${escapeHTML(error.message)}</span></div>`;
  } finally {
    button.disabled = false;
  }
}

function prepareClaimDialog() {
  document.querySelector('#claim-start-spread').textContent = '--';
  document.querySelector('#claim-winner').textContent = '--';
  document.querySelector('#claim-owner-count').textContent = '--';
  document.querySelector('#claim-stream').innerHTML = Array.from({length: 5}, (_, index) => `
    <div class="claim-row sending">
      <strong>demo-worker-${index + 1}</strong>
      <span class="claim-state">等待行锁</span>
      <span class="claim-session">--</span>
      <span class="claim-time">--</span>
    </div>
  `).join('');
  const verdict = document.querySelector('#claim-verdict');
  verdict.className = 'report-verdict waiting';
  verdict.innerHTML = '<span class="verdict-mark"></span><div><strong>正在竞争</strong><span>数据库正在确定唯一 owner</span></div>';
}

function renderClaimResults(payload) {
  const rows = [...document.querySelectorAll('#claim-stream .claim-row')];
  payload.attempts.forEach((attempt, index) => {
    const row = rows[index];
    row.className = attempt.won ? 'claim-row won' : 'claim-row lost';
    row.querySelector('.claim-state').textContent = attempt.won ? '认领成功' : '未获得任务';
    row.querySelector('.claim-session').textContent = `PID ${attempt.database_pid}`;
    row.querySelector('.claim-time').textContent = `${attempt.duration_ms.toFixed(1)} ms`;
  });
  document.querySelector('#claim-start-spread').textContent = `${payload.start_spread_ms.toFixed(2)} ms`;
  document.querySelector('#claim-winner').textContent = payload.winner.replace('demo-worker-', '#');
  document.querySelector('#claim-owner-count').textContent = payload.owner_count;
  const passed = payload.attempts.filter((attempt) => attempt.won).length === 1 && payload.owner_count === 1;
  const verdict = document.querySelector('#claim-verdict');
  verdict.className = passed ? 'report-verdict passed' : 'report-verdict failed';
  verdict.innerHTML = passed
    ? `<span class="verdict-mark">✓</span><div><strong>唯一认领验证通过</strong><span>${escapeHTML(payload.winner)} 成功，其余 4 个 Worker 未获得任务</span></div>`
    : '<span class="verdict-mark">!</span><div><strong>验证未通过</strong><span>任务出现多个 owner</span></div>';
}

async function reportFive(button) {
  const [taskID, position] = button.dataset.complete.split(':');
  const workerID = button.dataset.worker;
  const fencingToken = Number(button.dataset.token);
  button.disabled = true;
  prepareReportDialog(taskID, position);
  reportDialog.showModal();
  await new Promise((resolve) => setTimeout(resolve, 450));

  const batchStart = performance.now();
  const requests = [...document.querySelectorAll('#request-stream .request-row')].map((row, index) => {
    const startedAt = performance.now();
    const startOffset = startedAt - batchStart;
    row.className = 'request-row sending';
    row.querySelector('.request-state').textContent = '发送中';
    row.querySelector('.request-time').textContent = `+${startOffset.toFixed(2)} ms`;
    const response = apiFetch(
      `/api/tasks/${taskID}/steps/${position}/complete`,
      {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({success: true, worker_id: workerID, fencing_token: fencingToken}),
      },
    ).then(async (httpResponse) => {
      const payload = await httpResponse.json();
      if (!httpResponse.ok) throw new Error(payload.error || `请求 ${index + 1} 失败`);
      return {payload, duration: performance.now() - startedAt, startOffset};
    });
    return response;
  });

  const settled = await Promise.allSettled(requests);
  renderReportResults(settled);
  await refresh();
}

function prepareReportDialog(taskID, position) {
  document.querySelector('#report-target').textContent = `Task #${taskID} / Step ${position}`;
  document.querySelector('#report-start-spread').textContent = '--';
  document.querySelector('#report-duplicate-count').textContent = '--';
  document.querySelector('#report-log-count').textContent = '--';
  document.querySelector('#request-stream').innerHTML = Array.from({length: 5}, (_, index) => `
    <div class="request-row ready">
      <strong>Request ${index + 1}</strong>
      <span class="request-state">就绪</span>
      <span class="request-received">--</span>
      <span class="request-time">--</span>
    </div>
  `).join('');
  const verdict = document.querySelector('#report-verdict');
  verdict.className = 'report-verdict waiting';
  verdict.innerHTML = '<span class="verdict-mark"></span><div><strong>准备发送</strong><span>5 个请求将从浏览器同时发出</span></div>';
}

function renderReportResults(results) {
  const rows = [...document.querySelectorAll('#request-stream .request-row')];
  const successful = [];
  results.forEach((result, index) => {
    const row = rows[index];
    if (result.status === 'rejected') {
      row.className = 'request-row failed';
      row.querySelector('.request-state').textContent = '失败';
      row.querySelector('.request-received').textContent = '--';
      row.querySelector('.request-time').textContent = result.reason.message;
      return;
    }
    successful.push(result.value);
    const {payload, duration} = result.value;
    row.className = 'request-row complete';
    row.querySelector('.request-state').textContent = payload.duplicate_report ? '重复已去重' : '首次写入';
    row.querySelector('.request-received').textContent = formatPreciseTime(payload.received_at);
    row.querySelector('.request-time').textContent = `${duration.toFixed(1)} ms`;
  });

  const offsets = successful.map((result) => result.startOffset);
  const spread = offsets.length ? Math.max(...offsets) - Math.min(...offsets) : 0;
  const duplicateCount = successful.filter((result) => result.payload.duplicate_report).length;
  const logCount = successful.length ? Math.max(...successful.map((result) => result.payload.log_count)) : 0;
  document.querySelector('#report-start-spread').textContent = `${spread.toFixed(2)} ms`;
  document.querySelector('#report-duplicate-count').textContent = duplicateCount;
  document.querySelector('#report-log-count').textContent = logCount;

  const passed = successful.length === 5 && logCount === 1 && duplicateCount >= 4;
  const verdict = document.querySelector('#report-verdict');
  verdict.className = passed ? 'report-verdict passed' : 'report-verdict failed';
  verdict.innerHTML = passed
    ? '<span class="verdict-mark">✓</span><div><strong>并发幂等验证通过</strong><span>5 次完成请求，只保留 1 条执行日志</span></div>'
    : `<span class="verdict-mark">!</span><div><strong>验证未通过</strong><span>${successful.length} / 5 个请求成功，数据库日志 ${logCount} 条</span></div>`;
}

function formatPreciseTime(value) {
  const date = new Date(value);
  const base = date.toLocaleTimeString('zh-CN', {hour12: false});
  return `${base}.${String(date.getMilliseconds()).padStart(3, '0')}`;
}

function compactJSON(value) {
  return value && Object.keys(value).length ? JSON.stringify(value) : '--';
}

function escapeHTML(value) {
  return String(value)
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#039;');
}

function showToast(message, isError = false) {
  toast.textContent = message;
  toast.className = isError ? 'visible error' : 'visible';
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { toast.className = ''; }, 3200);
}

document.querySelectorAll('[data-add-param]').forEach((button) => {
  button.addEventListener('click', () => addParameterRow(document.querySelector(`#${button.dataset.addParam}`)));
});
document.querySelector('#open-create').addEventListener('click', () => {
  document.querySelector('#form-error').textContent = '';
  createDialog.showModal();
  document.querySelector('#group-name').focus();
});
document.querySelector('#close-create').addEventListener('click', () => createDialog.close());
document.querySelector('#cancel-create').addEventListener('click', () => createDialog.close());
createDialog.addEventListener('click', (event) => {
  if (event.target === createDialog) createDialog.close();
});
document.querySelector('#add-step').addEventListener('click', () => addStep([]));
document.querySelector('#open-claim-demo').addEventListener('click', (event) => runClaimDemo(event.currentTarget));
document.querySelector('#close-claim-demo').addEventListener('click', () => claimDialog.close());
document.querySelector('#finish-claim-demo').addEventListener('click', () => claimDialog.close());
document.querySelector('#close-report').addEventListener('click', () => reportDialog.close());
document.querySelector('#finish-report').addEventListener('click', () => reportDialog.close());
document.querySelector('#refresh-button').addEventListener('click', refresh);
document.querySelector('#previous-page').addEventListener('click', async () => {
  taskOffset = Math.max(0, taskOffset - taskPageSize);
  await refresh();
});
document.querySelector('#next-page').addEventListener('click', async () => {
  taskOffset += taskPageSize;
  await refresh();
});
addParameterRow(document.querySelector('#group-parameters'), {key: 'channel', value: 'sms'});
addParameterRow(document.querySelector('#group-parameters'), {key: 'region', value: 'east'});
addParameterRow(document.querySelector('#base-parameters'), {key: 'channel', value: 'email'});
addParameterRow(document.querySelector('#base-parameters'), {key: 'region', value: 'cn'});
addParameterRow(document.querySelector('#base-parameters'), {key: 'retries', value: '1', type: 'number'});
addStep([{key: 'sender', value: 'step-1'}]);
addStep([{key: 'sender', value: ''}, {key: 'retries', value: '2', type: 'number'}]);
bootstrap();
