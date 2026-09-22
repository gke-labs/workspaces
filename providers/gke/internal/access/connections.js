/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

'use strict';
const api = '/workspaces/connections/api';
const statusElement = document.getElementById('status');
const namespaceElement = document.getElementById('namespace');
const workspaceElement = document.getElementById('workspace');
const durationElement = document.getElementById('duration');
function durationLabel(seconds) {
  for (const [unit, length] of [['day', 86400], ['hour', 3600], ['minute', 60], ['second', 1]]) {
    if (seconds % length === 0) {
      const count = seconds / length;
      return `${count} ${unit}${count === 1 ? '' : 's'}`;
    }
  }
}
async function request(path, options = {}) {
  const response = await fetch(path, {credentials: 'same-origin', ...options});
  if (!response.ok) throw new Error(`Request failed (${response.status})`);
  return response.status === 204 ? null : response.json();
}
async function loadGrants() {
  const result = await request(api);
  document.getElementById('user').textContent = result.user;
  const previous = Number(durationElement.value);
  const policy = result.durationPolicy;
  const durations = [...new Set([result.minimumDurationSeconds, 3600, 28800, 86400, 604800, 1209600, 2592000, policy.defaultSeconds, policy.maxSeconds])]
    .filter(seconds => seconds >= result.minimumDurationSeconds && seconds <= policy.maxSeconds)
    .sort((left, right) => left - right);
  durationElement.replaceChildren();
  for (const seconds of durations) {
    durationElement.add(new Option(durationLabel(seconds) + (seconds === policy.defaultSeconds ? ' (default)' : ''), String(seconds)));
  }
  durationElement.value = String(durations.includes(previous) ? previous : policy.defaultSeconds);
  const body = document.getElementById('grants');
  body.replaceChildren();
  for (const grant of result.grants) {
    const row = body.insertRow();
    for (const value of [`${grant.namespace}/${grant.workspace}`, new Date(grant.expiresAt).toLocaleString(), grant.id]) row.insertCell().textContent = value;
    const revoke = document.createElement('button');
    revoke.type = 'button';
    revoke.textContent = 'Revoke';
    revoke.onclick = async () => {
      try {
        await request(`${api}/${encodeURIComponent(grant.id)}`, {method: 'DELETE'});
        document.getElementById('url').value = '';
        document.getElementById('result').hidden = true;
        statusElement.textContent = 'Connection revoked';
        await loadGrants();
      } catch (error) { statusElement.textContent = error.message; }
    };
    row.insertCell().append(revoke);
  }
}
async function loadWorkspaces() {
  workspaceElement.replaceChildren();
  const result = await request(`/workspaces/api/v1/workspaces/${encodeURIComponent(namespaceElement.value)}`);
  for (const workspace of result.data) {
    if (!workspace.paused && workspace.state === 'Running') workspaceElement.add(new Option(workspace.name, workspace.name));
  }
}
namespaceElement.onchange = () => loadWorkspaces().catch(error => { statusElement.textContent = error.message; });
document.getElementById('connection').onsubmit = async event => {
  event.preventDefault();
  const button = document.getElementById('generate');
  button.disabled = true;
  try {
    const durationSeconds = Number(durationElement.value);
    const result = await request(api, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({namespace: namespaceElement.value, workspace: workspaceElement.value, port: document.getElementById('port').value, durationSeconds})});
    document.getElementById('url').value = result.url;
    document.getElementById('result').hidden = false;
    statusElement.textContent = `Requested ${durationLabel(durationSeconds)}. Expires ${new Date(result.grant.expiresAt).toLocaleString()}`;
    await loadGrants();
  } catch (error) { statusElement.textContent = error.message; }
  finally { button.disabled = false; }
};
document.getElementById('copy').onclick = async () => {
  try { await navigator.clipboard.writeText(document.getElementById('url').value); statusElement.textContent = 'Connection URL copied'; }
  catch { statusElement.textContent = 'Clipboard unavailable'; }
};
async function initialize() {
  await loadGrants();
  const result = await request('/workspaces/api/v1/namespaces');
  for (const namespace of result.data) namespaceElement.add(new Option(namespace.name, namespace.name));
  if (namespaceElement.value) await loadWorkspaces();
  document.getElementById('generate').disabled = false;
}
initialize().catch(error => { statusElement.textContent = error.message; });