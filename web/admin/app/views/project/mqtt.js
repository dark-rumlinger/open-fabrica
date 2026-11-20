/**
 * Project MQTT Inspector: broker toggle, live SSE topic monitor, test
 * publisher, device credentials/watchdogs, outbound LAN bridges, ingest
 * rules, and retained topics.
 */

import { h, render, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { createScope } from '../../core/lifecycle.js';
import { get, post, put, del } from '../../core/http.js';
import * as state from '../../core/state.js';
import * as toast from '../../ui/toast.js';
import * as modal from '../../ui/modal.js';
import { humanizeError } from '../../ui/errors.js';

const MAX_LIVE_EVENTS = 100;

export function renderProjectMQTT(container, projectId) {
  const scope = createScope();
  let liveEvents = [];
  let paused = false;
  let data = null;

  const root = h('div', { className: 'page-stack' });
  render(container, root);

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
        style: { display: 'flex', gap: '8px', alignItems: 'baseline', padding: '4px 6px', borderBottom: '1px solid var(--border-subtle)' },
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
      if (!scope.disposed) toast.error(humanizeError(err.message));
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

  function openAddDeviceModal() {
    const nameIn = h('input', { className: 'input', placeholder: 'greenhouse_esp32' });
    const userIn = h('input', { className: 'input', placeholder: 'esp1' });
    const pubIn = h('input', { className: 'input', value: '#' });
    const subIn = h('input', { className: 'input', value: '#' });
    const hbIn = h('input', { className: 'input', type: 'number', min: '0', value: '0', placeholder: '300' });

    const body = h('div', { className: 'flex-col', style: { gap: '10px' } }, [
      h('label', { className: 'field' }, [h('span', { className: 'field__label' }, 'Name'), nameIn]),
      h('label', { className: 'field' }, [h('span', { className: 'field__label' }, 'Username'), userIn]),
      h('label', { className: 'field' }, [h('span', { className: 'field__label' }, 'Allow Publish (CSV)'), pubIn]),
      h('label', { className: 'field' }, [h('span', { className: 'field__label' }, 'Allow Subscribe (CSV)'), subIn]),
      h('label', { className: 'field' }, [h('span', { className: 'field__label' }, 'Heartbeat Watchdog (sec, 0 = off)'), hbIn]),
    ]);

    modal.show(t('mqtt.addDevice'), body, [
      modal.cancelBtn(),
      h('button', {
        className: 'btn btn--primary',
        onClick: async () => {
          try {
            const res = await post(`/admin/api/projects/${projectId}/mqtt/devices`, {
              name: nameIn.value.trim(),
              username: userIn.value.trim(),
              allow_pub: pubIn.value.trim() || '#',
              allow_sub: subIn.value.trim() || '#',
              heartbeat_sec: parseInt(hbIn.value, 10) || 0,
            });
            modal.close();
            if (res?.token) {
              modal.show(t('mqtt.addDevice'), h('div', { className: 'flex-col', style: { gap: '8px' } }, [
                h('p', { className: 'text-secondary' }, t('mqtt.tokenOnce')),
                h('code', { className: 'font-mono', style: { padding: '8px', background: 'var(--bg-subtle)', borderRadius: '6px', wordBreak: 'break-all' } }, res.token),
              ]), [modal.cancelBtn(t('common.close'))]);
            }
            await load();
          } catch (err) {
            toast.error(humanizeError(err.message));
          }
        },
      }, t('common.save')),
    ]);
  }

  function openAddBridgeModal() {
    const nameIn = h('input', { className: 'input', placeholder: 'home_mosquitto' });
    const urlIn = h('input', { className: 'input', placeholder: 'tcp://192.168.1.50:1883' });
    const rSubIn = h('input', { className: 'input', placeholder: 'zigbee2mqtt/#,frigate/events' });
    const lPreIn = h('input', { className: 'input', placeholder: 'lan/' });
    const lPubIn = h('input', { className: 'input', placeholder: 'lan/zigbee2mqtt/+/set' });
    const userIn = h('input', { className: 'input', placeholder: 'Optional username' });
    const passIn = h('input', { className: 'input', type: 'password', placeholder: 'Optional password' });

    const body = h('div', { className: 'flex-col', style: { gap: '10px' } }, [
      h('label', { className: 'field' }, [h('span', { className: 'field__label' }, 'Name'), nameIn]),
      h('label', { className: 'field' }, [h('span', { className: 'field__label' }, 'Broker URL (tcp:// or tls://)'), urlIn]),
      h('label', { className: 'field' }, [h('span', { className: 'field__label' }, 'Remote Subscribe (CSV)'), rSubIn]),
      h('label', { className: 'field' }, [h('span', { className: 'field__label' }, 'Local Prefix'), lPreIn]),
      h('label', { className: 'field' }, [h('span', { className: 'field__label' }, 'Local Publish Out (CSV)'), lPubIn]),
      h('label', { className: 'field' }, [h('span', { className: 'field__label' }, 'Username'), userIn]),
      h('label', { className: 'field' }, [h('span', { className: 'field__label' }, 'Password'), passIn]),
    ]);

    modal.show(t('mqtt.addBridge'), body, [
      modal.cancelBtn(),
      h('button', {
        className: 'btn btn--primary',
        onClick: async () => {
          try {
            await post(`/admin/api/projects/${projectId}/mqtt/bridges`, {
              name: nameIn.value.trim(),
              broker_url: urlIn.value.trim(),
              remote_sub: rSubIn.value.trim(),
              local_prefix: lPreIn.value.trim(),
              local_pub: lPubIn.value.trim(),
              username: userIn.value.trim(),
              password: passIn.value,
              qos: 1,
            });
            modal.close();
            await load();
          } catch (err) {
            toast.error(humanizeError(err.message));
          }
        },
      }, t('common.save')),
    ]);
  }

  function paint() {
    clear(root);
    const cfg = data.config || {};
    const liveClients = new Set((data.clients || []).map(c => c.username || c.device));
    const bridgeLive = new Map((data.bridge_status || []).map(b => [b.name, b]));

    // 1. Header & Broker Status
    const header = h('div', { className: 'card' }, [
      h('div', { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: '12px' } }, [
        h('div', {}, [
          h('h2', { style: { margin: 0 } }, t('mqtt.title')),
          h('p', { className: 'text-secondary', style: { margin: '4px 0 0' } }, t('mqtt.subtitle')),
        ]),
        h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, [
          h('span', { className: cfg.enabled ? 'badge badge--success' : 'badge' }, cfg.enabled ? t('mqtt.enabled') : t('mqtt.disabled')),
          h('span', { className: 'badge' }, `${t('mqtt.connectedClients')}: ${data.connected_clients || 0}`),
          h('span', { className: 'badge font-mono' }, cfg.ws_path || '/mqtt'),
          ...(cfg.tcp_port ? [h('span', { className: 'badge font-mono' }, `TCP :${cfg.tcp_port}`)] : []),
          h('button', { className: 'btn btn--sm btn--secondary', onClick: load }, t('common.refresh')),
          h('button', { className: `btn btn--sm ${cfg.enabled ? 'btn--secondary' : 'btn--primary'}`, onClick: toggleEnabled }, cfg.enabled ? t('mqtt.disable') : t('mqtt.enable')),
        ]),
      ]),
    ]);

    // 2. Test Publisher
    const topicIn = h('input', { className: 'input font-mono', placeholder: 'sensors/esp1/temp', style: { flex: '1 1 200px' } });
    const payloadIn = h('input', { className: 'input font-mono', placeholder: '{"c": 22.5}', style: { flex: '2 1 260px' } });
    const retainChk = h('input', { type: 'checkbox' });
    const pubCard = h('div', { className: 'card' }, [
      h('h3', { style: { marginTop: 0 } }, t('mqtt.testPublish')),
      h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' } }, [
        topicIn,
        payloadIn,
        h('label', { style: { display: 'flex', gap: '4px', alignItems: 'center' } }, [retainChk, t('mqtt.retain')]),
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
    ]);

    // 3. Live Topic Monitor
    const pauseBtn = h('button', {
      className: 'btn btn--sm btn--secondary',
      onClick: () => {
        paused = !paused;
        pauseBtn.textContent = paused ? t('mqtt.resume') : t('mqtt.pause');
      },
    }, paused ? t('mqtt.resume') : t('mqtt.pause'));
    renderLiveList();
    const liveCard = h('div', { className: 'card' }, [
      h('div', { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' } }, [
        h('h3', { style: { margin: 0 } }, t('mqtt.liveStream')),
        h('div', { style: { display: 'flex', gap: '6px' } }, [
          pauseBtn,
          h('button', {
            className: 'btn btn--sm btn--secondary',
            onClick: () => { liveEvents = []; renderLiveList(); },
          }, t('mqtt.clear')),
        ]),
      ]),
      liveListEl,
    ]);

    // 4. Devices
    const devicesCard = h('div', { className: 'card' }, [
      h('div', { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' } }, [
        h('h3', { style: { margin: 0 } }, t('mqtt.devices')),
        h('button', { className: 'btn btn--sm btn--primary', onClick: openAddDeviceModal }, `+ ${t('mqtt.addDevice')}`),
      ]),
      !(data.devices || []).length
        ? h('div', { className: 'text-secondary' }, t('mqtt.devicesEmpty'))
        : h('table', { className: 'table' }, [
            h('thead', {}, [h('tr', {}, [
              h('th', {}, 'Name'), h('th', {}, 'Username'), h('th', {}, 'ACL (Pub / Sub)'),
              h('th', {}, 'Heartbeat'), h('th', {}, 'Status'), h('th', {}, ''),
            ])]),
            h('tbody', {}, (data.devices || []).map(d => {
              const online = liveClients.has(d.username);
              const badge = d.stale
                ? h('span', { className: 'badge badge--danger' }, 'stale')
                : online
                  ? h('span', { className: 'badge badge--success' }, t('mqtt.connected'))
                  : h('span', { className: 'badge' }, d.last_seen_at ? d.last_seen_at.replace('T', ' ').replace('Z', '') : '—');
              return h('tr', {}, [
                h('td', { className: 'font-mono' }, d.name),
                h('td', { className: 'font-mono' }, d.username),
                h('td', { className: 'font-mono text-sm' }, `${d.allow_pub} / ${d.allow_sub}`),
                h('td', {}, d.heartbeat_sec > 0 ? `${d.heartbeat_sec}s` : '—'),
                h('td', {}, [badge]),
                h('td', { style: { textAlign: 'right' } }, [
                  h('button', {
                    className: 'btn btn--sm btn--secondary',
                    onClick: async () => {
                      await del(`/admin/api/projects/${projectId}/mqtt/devices/${encodeURIComponent(d.name)}`);
                      await load();
                    },
                  }, t('common.delete')),
                ]),
              ]);
            })),
          ]),
    ]);

    // 5. Bridges
    const bridgesCard = h('div', { className: 'card' }, [
      h('div', { style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' } }, [
        h('h3', { style: { margin: 0 } }, t('mqtt.bridges')),
        h('button', { className: 'btn btn--sm btn--primary', onClick: openAddBridgeModal }, `+ ${t('mqtt.addBridge')}`),
      ]),
      !(data.bridges || []).length
        ? h('div', { className: 'text-secondary' }, t('mqtt.bridgesEmpty'))
        : h('table', { className: 'table' }, [
            h('thead', {}, [h('tr', {}, [
              h('th', {}, 'Name'), h('th', {}, 'Broker URL'), h('th', {}, 'In / Out'), h('th', {}, 'Status'), h('th', {}, ''),
            ])]),
            h('tbody', {}, (data.bridges || []).map(b => {
              const live = bridgeLive.get(b.name);
              const badge = live?.connected
                ? h('span', { className: 'badge badge--success' }, `${t('mqtt.connected')} (${live.messages_in}/${live.messages_out})`)
                : h('span', { className: live?.last_error ? 'badge badge--danger' : 'badge', title: live?.last_error || '' }, live?.last_error ? live.last_error.slice(0, 48) : t('mqtt.disconnected'));
              return h('tr', {}, [
                h('td', { className: 'font-mono' }, b.name),
                h('td', { className: 'font-mono text-sm' }, b.broker_url),
                h('td', { className: 'font-mono text-sm' }, `${b.remote_sub || '—'} / ${b.local_pub || '—'}`),
                h('td', {}, [badge]),
                h('td', { style: { textAlign: 'right' } }, [
                  h('button', {
                    className: 'btn btn--sm btn--secondary',
                    onClick: async () => {
                      await del(`/admin/api/projects/${projectId}/mqtt/bridges/${encodeURIComponent(b.name)}`);
                      await load();
                    },
                  }, t('common.delete')),
                ]),
              ]);
            })),
          ]),
    ]);

    // 6. Rules & Retained
    const rulesCard = h('div', { className: 'card' }, [
      h('h3', { style: { marginTop: 0 } }, t('mqtt.rules')),
      !(data.rules || []).length
        ? h('div', { className: 'text-secondary' }, t('mqtt.rulesEmpty'))
        : h('table', { className: 'table' }, [
            h('thead', {}, [h('tr', {}, [
              h('th', {}, 'Name'), h('th', {}, 'Topic Filter'), h('th', {}, 'Target'), h('th', {}, 'Rollup / Retention'),
            ])]),
            h('tbody', {}, (data.rules || []).map(r => h('tr', {}, [
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
    ]);

    const retainedCard = h('div', { className: 'card' }, [
      h('h3', { style: { marginTop: 0 } }, t('mqtt.retained')),
      !(data.retained || []).length
        ? h('div', { className: 'text-secondary' }, t('mqtt.retainedEmpty'))
        : h('table', { className: 'table' }, [
            h('thead', {}, [h('tr', {}, [h('th', {}, t('mqtt.topic')), h('th', {}, t('mqtt.payload'))])]),
            h('tbody', {}, (data.retained || []).map(rm => h('tr', {}, [
              h('td', { className: 'font-mono text-sm' }, rm.topic),
              h('td', { className: 'font-mono text-sm', style: { wordBreak: 'break-all' } }, String(rm.payload || '').slice(0, 200)),
            ]))),
          ]),
    ]);

    root.append(header, pubCard, liveCard, devicesCard, bridgesCard, rulesCard, retainedCard);
  }

  load();
  return () => scope.dispose();
}
