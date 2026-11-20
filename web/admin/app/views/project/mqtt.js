/**
 * Project MQTT Inspector: broker toggle, live SSE topic monitor, test
 * publisher, device credentials/watchdogs, outbound LAN bridges, ingest
 * rules, and retained topics.
 *
 * Built on the shared page scaffold (pageHeader + context-panel body, section
 * headers, card() item lists, createFormModal) so it matches the other project
 * tabs. Behaviour and API calls are unchanged.
 */

import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { createScope } from '../../core/lifecycle.js';
import { get, post, put, del } from '../../core/http.js';
import * as state from '../../core/state.js';
import * as toast from '../../ui/toast.js';
import * as modal from '../../ui/modal.js';
import { humanizeError } from '../../ui/errors.js';
import { pageHeader, card } from '../../ui/card.js';
import { createFormModal } from '../../ui/form-modal.js';
import { formGroup, emptyState, showLoadError, listSkeleton } from '../../ui/helpers.js';
import { icon } from '../../ui/icon.js';
import { formatDateTime } from '../../core/datetime.js';

const MAX_LIVE_EVENTS = 100;

export function renderProjectMQTT(container, projectId) {
  const scope = createScope();
  let liveEvents = [];
  let paused = false;
  let data = null;

  // The live-event list persists across repaints; it is re-parented into a
  // fresh card each paint while the SSE handler keeps updating it in place.
  const liveListEl = h('div', {
    className: 'font-mono text-sm',
    style: { maxHeight: '260px', overflowY: 'auto', display: 'flex', flexDirection: 'column', gap: '4px' },
  });

  function renderLiveList() {
    clear(liveListEl);
    if (!liveEvents.length) {
      liveListEl.appendChild(h('div', { className: 'text-secondary' }, t('mqtt.liveEmpty')));
      return;
    }
    for (const ev of liveEvents) {
      const ts = String(ev.timestamp || '').slice(11, 19);
      const badgeCls = ev.type === 'mqtt.device_stale'
        ? 'badge badge--danger'
        : ev.type === 'mqtt.client_connected' || ev.type === 'mqtt.device_recovered'
          ? 'badge badge--success'
          : 'badge';
      const detail = ev.type === 'mqtt.publish'
        ? `${ev.payload.topic || ''} -> ${String(ev.payload.payload || '').slice(0, 160)}`
        : `${ev.payload.device || ev.payload.client_id || ''} ${ev.payload.heartbeat_sec ? `(${ev.payload.heartbeat_sec}s)` : ''}`.trim();
      liveListEl.appendChild(h('div', {
        className: 'flex gap-2',
        style: { alignItems: 'baseline', padding: '4px 6px', borderBottom: '1px solid var(--border)' },
      }, [
        h('span', { className: 'text-secondary', style: { flexShrink: 0 } }, ts),
        h('span', { className: badgeCls, style: { flexShrink: 0 } }, ev.type.replace('mqtt.', '')),
        h('span', { style: { wordBreak: 'break-all' } }, detail),
      ]));
    }
  }

  state.select('mqttEvent', (ev) => {
    if (!ev || ev.project_id !== projectId || paused) return;
    liveEvents = [ev, ...liveEvents].slice(0, MAX_LIVE_EVENTS);
    renderLiveList();
  }, scope);

  async function load() {
    try {
      data = await get(`/admin/api/projects/${projectId}/mqtt`);
      if (!scope.disposed) paint();
    } catch (err) {
      if (!scope.disposed) showLoadError(container, t('mqtt.title'), err, load);
    }
  }

  async function toggleEnabled() {
    const cfg = data.config;
    try {
      await put(`/admin/api/projects/${projectId}/mqtt/config`, {
        enabled: !cfg.enabled,
        allow_anonymous: cfg.allow_anonymous,
        max_clients: cfg.max_clients,
        user_allow_pub: cfg.user_allow_pub,
        user_allow_sub: cfg.user_allow_sub,
      });
      await load();
    } catch (err) {
      toast.error(humanizeError(err.message));
    }
  }

  // Section divider: an icon + title on the left, an optional control on the
  // right (an Add button, the live-stream pause/clear). Matches the diagnostics
  // tab's section headers.
  function sectionHeader(title, iconName, right) {
    return h('div', { className: 'flex items-center justify-between mb-2 mt-4' }, [
      h('h4', { className: 'flex items-center gap-2 text-sm text-secondary', style: { margin: 0 } }, [
        h('span', { innerHTML: icon(iconName) }),
        h('span', {}, title),
      ]),
      right || h('span'),
    ]);
  }

  function openAddDeviceModal() {
    const nameIn = h('input', { className: 'input', placeholder: 'greenhouse_esp32' });
    const userIn = h('input', { className: 'input', placeholder: 'esp1' });
    const pubIn = h('input', { className: 'input', value: '#' });
    const subIn = h('input', { className: 'input', value: '#' });
    const hbIn = h('input', { className: 'input', type: 'number', min: '0', value: '0', placeholder: '300' });
    let token = null;

    createFormModal({
      title: t('mqtt.addDevice'),
      fields: [
        formGroup('Name', nameIn),
        formGroup('Username', userIn),
        formGroup('Allow Publish (CSV)', pubIn),
        formGroup('Allow Subscribe (CSV)', subIn),
        formGroup('Heartbeat Watchdog (sec, 0 = off)', hbIn),
      ],
      submit: async () => {
        const res = await post(`/admin/api/projects/${projectId}/mqtt/devices`, {
          name: nameIn.value.trim(),
          username: userIn.value.trim(),
          allow_pub: pubIn.value.trim() || '#',
          allow_sub: subIn.value.trim() || '#',
          heartbeat_sec: parseInt(hbIn.value, 10) || 0,
        });
        token = res?.token || null;
      },
      onDone: () => {
        // The token is returned once; this is the owner's only chance to copy it.
        if (token) {
          modal.show(t('mqtt.addDevice'), h('div', { className: 'flex-col', style: { gap: '8px' } }, [
            h('p', { className: 'text-secondary' }, t('mqtt.tokenOnce')),
            h('code', { className: 'font-mono', style: { padding: '8px', background: 'var(--bg-secondary)', borderRadius: '6px', wordBreak: 'break-all' } }, token),
          ]), [modal.cancelBtn(t('common.close'))]);
        }
        load();
      },
    });
  }

  function openAddBridgeModal() {
    const nameIn = h('input', { className: 'input', placeholder: 'home_mosquitto' });
    const urlIn = h('input', { className: 'input', placeholder: 'tcp://192.168.1.50:1883' });
    const rSubIn = h('input', { className: 'input', placeholder: 'zigbee2mqtt/#,frigate/events' });
    const lPreIn = h('input', { className: 'input', placeholder: 'lan/' });
    const lPubIn = h('input', { className: 'input', placeholder: 'lan/zigbee2mqtt/+/set' });
    const userIn = h('input', { className: 'input', placeholder: 'Optional username' });
    const passIn = h('input', { className: 'input', type: 'password', placeholder: 'Optional password' });

    createFormModal({
      title: t('mqtt.addBridge'),
      fields: [
        formGroup('Name', nameIn),
        formGroup('Broker URL (tcp:// or tls://)', urlIn),
        formGroup('Remote Subscribe (CSV)', rSubIn),
        formGroup('Local Prefix', lPreIn),
        formGroup('Local Publish Out (CSV)', lPubIn),
        formGroup('Username', userIn),
        formGroup('Password', passIn),
      ],
      submit: () => post(`/admin/api/projects/${projectId}/mqtt/bridges`, {
        name: nameIn.value.trim(),
        broker_url: urlIn.value.trim(),
        remote_sub: rSubIn.value.trim(),
        local_prefix: lPreIn.value.trim(),
        local_pub: lPubIn.value.trim(),
        username: userIn.value.trim(),
        password: passIn.value,
        qos: 1,
      }),
      onDone: load,
    });
  }

  function deviceCard(d, liveClients) {
    const online = liveClients.has(d.username);
    const badge = d.stale
      ? h('span', { className: 'badge badge--danger' }, 'stale')
      : online
        ? h('span', { className: 'badge badge--success' }, t('mqtt.connected'))
        : h('span', { className: 'badge' }, d.last_seen_at ? formatDateTime(d.last_seen_at) : '—');

    return card({
      title: d.name,
      iconName: 'radio',
      badge,
      children: [
        h('div', { className: 'text-sm text-secondary mt-2' }, [
          h('span', {}, 'User '), h('code', { className: 'font-mono' }, d.username || '—'),
          h('span', {}, '  ·  ACL '), h('code', { className: 'font-mono' }, `${d.allow_pub} / ${d.allow_sub}`),
          ...(d.heartbeat_sec > 0 ? [h('span', {}, '  ·  HB '), h('code', { className: 'font-mono' }, `${d.heartbeat_sec}s`)] : []),
        ]),
      ],
      actions: [{
        label: t('common.delete'), kind: 'danger', iconName: 'trash',
        onClick: async () => {
          await del(`/admin/api/projects/${projectId}/mqtt/devices/${encodeURIComponent(d.name)}`);
          await load();
        },
      }],
    });
  }

  function bridgeCard(b, bridgeLive) {
    const live = bridgeLive.get(b.name);
    const badge = live?.connected
      ? h('span', { className: 'badge badge--success' }, `${t('mqtt.connected')} (${live.messages_in}/${live.messages_out})`)
      : h('span', { className: live?.last_error ? 'badge badge--danger' : 'badge', title: live?.last_error || '' },
          live?.last_error ? live.last_error.slice(0, 48) : t('mqtt.disconnected'));

    return card({
      title: b.name,
      iconName: 'link',
      badge,
      children: [
        h('div', { className: 'text-sm text-secondary mt-2' }, [
          h('span', {}, 'URL '), h('code', { className: 'font-mono', style: { wordBreak: 'break-all' } }, b.broker_url),
        ]),
        h('div', { className: 'text-sm text-secondary mt-1' }, [
          h('span', {}, 'In '), h('code', { className: 'font-mono' }, b.remote_sub || '—'),
          h('span', {}, '  ·  Out '), h('code', { className: 'font-mono' }, b.local_pub || '—'),
        ]),
      ],
      actions: [{
        label: t('common.delete'), kind: 'danger', iconName: 'trash',
        onClick: async () => {
          await del(`/admin/api/projects/${projectId}/mqtt/bridges/${encodeURIComponent(b.name)}`);
          await load();
        },
      }],
    });
  }

  function paint() {
    clear(container);
    const cfg = data.config || {};
    const liveClients = new Set((data.clients || []).map(c => c.username || c.device));
    const bridgeLive = new Map((data.bridge_status || []).map(b => [b.name, b]));

    container.appendChild(pageHeader({
      title: t('mqtt.title'),
      actions: [
        { label: t('common.refresh'), kind: 'secondary', onClick: load },
        {
          label: cfg.enabled ? t('mqtt.disable') : t('mqtt.enable'),
          kind: cfg.enabled ? 'secondary' : 'primary',
          onClick: toggleEnabled,
        },
      ],
    }));

    const body = h('div', { className: 'context-panel__body--page' });

    // Broker status strip.
    body.appendChild(h('div', { className: 'card mb-3' }, [
      h('div', { className: 'flex items-center gap-2 flex-wrap' }, [
        h('span', { className: cfg.enabled ? 'badge badge--success' : 'badge' }, cfg.enabled ? t('mqtt.enabled') : t('mqtt.disabled')),
        h('span', { className: 'badge' }, `${t('mqtt.connectedClients')}: ${data.connected_clients || 0}`),
        h('span', { className: 'badge font-mono' }, cfg.ws_path || '/mqtt'),
        ...(cfg.tcp_port ? [h('span', { className: 'badge font-mono' }, `TCP :${cfg.tcp_port}`)] : []),
      ]),
    ]));

    // Test Publisher.
    const topicIn = h('input', { className: 'input font-mono', placeholder: 'sensors/esp1/temp', style: { flex: '1 1 200px' } });
    const payloadIn = h('input', { className: 'input font-mono', placeholder: '{"c": 22.5}', style: { flex: '2 1 260px' } });
    const retainChk = h('input', { type: 'checkbox' });
    body.appendChild(sectionHeader(t('mqtt.testPublish'), 'send'));
    body.appendChild(h('div', { className: 'card mb-3' }, [
      h('div', { className: 'flex items-center gap-2 flex-wrap' }, [
        topicIn,
        payloadIn,
        h('label', { className: 'flex items-center gap-1' }, [retainChk, t('mqtt.retain')]),
        h('button', {
          className: 'btn btn--sm btn--primary',
          disabled: !cfg.enabled,
          onClick: async () => {
            try {
              await post(`/admin/api/projects/${projectId}/mqtt/publish`, {
                topic: topicIn.value.trim(),
                payload: payloadIn.value,
                qos: 1,
                retain: retainChk.checked,
              });
              toast.success(t('mqtt.published'));
              if (retainChk.checked) await load();
            } catch (err) {
              toast.error(humanizeError(err.message));
            }
          },
        }, t('mqtt.publish')),
      ]),
    ]));

    // Live Topic Stream.
    const pauseBtn = h('button', {
      className: 'btn btn--sm btn--secondary',
      onClick: () => {
        paused = !paused;
        pauseBtn.textContent = paused ? t('mqtt.resume') : t('mqtt.pause');
      },
    }, paused ? t('mqtt.resume') : t('mqtt.pause'));
    const clearBtn = h('button', {
      className: 'btn btn--sm btn--secondary',
      onClick: () => { liveEvents = []; renderLiveList(); },
    }, t('mqtt.clear'));
    body.appendChild(sectionHeader(t('mqtt.liveStream'), 'activity', h('div', { className: 'flex items-center gap-2' }, [pauseBtn, clearBtn])));
    renderLiveList();
    body.appendChild(h('div', { className: 'card mb-3' }, [liveListEl]));

    // Devices.
    const addDeviceBtn = h('button', { className: 'btn btn--primary btn--sm', onClick: openAddDeviceModal },
      [h('span', { innerHTML: icon('plus') }), ' ' + t('mqtt.addDevice')]);
    body.appendChild(sectionHeader(t('mqtt.devices'), 'radio', addDeviceBtn));
    if (!(data.devices || []).length) {
      body.appendChild(emptyState({ iconName: 'radio', title: t('mqtt.devices'), description: t('mqtt.devicesEmpty') }));
    } else {
      for (const d of data.devices) body.appendChild(deviceCard(d, liveClients));
    }

    // Outbound LAN Bridges.
    const addBridgeBtn = h('button', { className: 'btn btn--primary btn--sm', onClick: openAddBridgeModal },
      [h('span', { innerHTML: icon('plus') }), ' ' + t('mqtt.addBridge')]);
    body.appendChild(sectionHeader(t('mqtt.bridges'), 'link', addBridgeBtn));
    if (!(data.bridges || []).length) {
      body.appendChild(emptyState({ iconName: 'link', title: t('mqtt.bridges'), description: t('mqtt.bridgesEmpty') }));
    } else {
      for (const b of data.bridges) body.appendChild(bridgeCard(b, bridgeLive));
    }

    // Ingest Rules & Rollups.
    body.appendChild(sectionHeader(t('mqtt.rules'), 'zap'));
    if (!(data.rules || []).length) {
      body.appendChild(emptyState({ iconName: 'zap', title: t('mqtt.rules'), description: t('mqtt.rulesEmpty') }));
    } else {
      body.appendChild(h('div', { className: 'card mb-3' }, [
        h('table', { className: 'table' }, [
          h('thead', {}, [h('tr', {}, [
            h('th', {}, 'Name'), h('th', {}, 'Topic Filter'), h('th', {}, 'Target'), h('th', {}, 'Rollup / Retention'),
          ])]),
          h('tbody', {}, data.rules.map(r => h('tr', {}, [
            h('td', { className: 'font-mono' }, r.name),
            h('td', { className: 'font-mono text-sm' }, r.topic_filter),
            h('td', { className: 'font-mono text-sm' }, [r.target_table, r.function_path].filter(Boolean).join(' + ') || '—'),
            h('td', { className: 'text-sm' }, [
              r.rollup_interval ? `rollup ${r.rollup_interval}` : '',
              r.retention_days > 0 ? `${r.retention_days}d` : '',
              r.min_interval_sec > 0 ? `≥${r.min_interval_sec}s` : '',
            ].filter(Boolean).join(', ') || '—'),
          ]))),
        ]),
      ]));
    }

    // Retained Messages.
    body.appendChild(sectionHeader(t('mqtt.retained'), 'database'));
    if (!(data.retained || []).length) {
      body.appendChild(emptyState({ iconName: 'database', title: t('mqtt.retained'), description: t('mqtt.retainedEmpty') }));
    } else {
      body.appendChild(h('div', { className: 'card mb-3' }, [
        h('table', { className: 'table' }, [
          h('thead', {}, [h('tr', {}, [h('th', {}, t('mqtt.topic')), h('th', {}, t('mqtt.payload'))])]),
          h('tbody', {}, data.retained.map(rm => h('tr', {}, [
            h('td', { className: 'font-mono text-sm' }, rm.topic),
            h('td', { className: 'font-mono text-sm', style: { wordBreak: 'break-all' } }, String(rm.payload || '').slice(0, 200)),
          ]))),
        ]),
      ]));
    }

    container.appendChild(body);
  }

  // Initial frame: the page header plus a skeleton, so the panel reads as
  // "loading" rather than blank until the first fetch lands.
  clear(container);
  container.appendChild(pageHeader({ title: t('mqtt.title') }));
  const loadingBody = h('div', { className: 'context-panel__body--page' }, [listSkeleton()]);
  container.appendChild(loadingBody);

  load();
  return () => scope.dispose();
}
