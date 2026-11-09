/**
 * Project settings tab: name, domain, model, sharing and status, advanced options, danger zone.
 */

import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, put, del, download } from '../../core/http.js';
import { createScope } from '../../core/lifecycle.js';
import { navigate, currentPath } from '../../core/router.js';
import { icon } from '../../ui/icon.js';
import * as toast from '../../ui/toast.js';
import * as modal from '../../ui/modal.js';
import * as state from '../../core/state.js';
import { createModelPicker } from '../../ui/model-picker.js';
import { createTabbedPanel } from '../../ui/tabbed-panel.js';
import { formGroup } from '../../ui/helpers.js';
import { humanizeError } from '../../ui/errors.js';
import { createHostingChip } from '../../ui/hosting-status.js';
import { loadFirewallNotice } from '../../ui/firewall-notice.js';
import { loadRouterForwardingNotice } from '../../ui/router-forwarding.js';
import { createGoLiveTasks } from './go-live.js';
import { createPublishStrip } from '../../ui/publish-status.js';
import { createEngineCard } from './engine-card.js';
import { createSpendCard } from './spend-card.js';

// Unsaved edits per project. General, Sharing and Advanced are separate routes,
// so every section switch remounts this panel; without somewhere to keep the
// inputs, an edited domain would be silently dropped on the way to Advanced.
// Cleared on save and when the person leaves Settings altogether.
const drafts = new Map();

export async function renderProjectSettings(container, projectId, project, section) {
  clear(container);
  const scope = createScope();

  const draft = drafts.get(projectId) || {};
  const remember = (key, input, read = (el) => el.value) => {
    const store = () => { drafts.set(projectId, { ...(drafts.get(projectId) || {}), [key]: read(input) }); };
    input.addEventListener('input', store);
    input.addEventListener('change', store);
  };

  const readOnly = !state.isAdmin();
  const nameInput = h('input', { className: 'input', value: draft.name ?? project.name, disabled: readOnly, required: true });
  remember('name', nameInput);
  const domainInput = h('input', { className: 'input', value: draft.domain ?? (project.domain || ''), placeholder: 'example.com', disabled: readOnly, pattern: '[a-zA-Z0-9][a-zA-Z0-9.\\-]*' });
  remember('domain', domainInput);
  const descInput = h('textarea', {
    className: 'input',
    rows: '3',
    value: draft.description ?? (project.description || ''),
    placeholder: t('set.descPlaceholder'),
    disabled: readOnly,
  });

  // Advisory domain pre-flight: checks DNS/HTTPS configuration without
  // blocking the save. Skipped for empty fields and .localhost dev domains.
  const domainStatus = h('p', { className: 'form-hint' });
  const checkDomainBtn = h('button', {
    className: 'btn btn--secondary',
    type: 'button',
    disabled: readOnly,
    onClick: async () => {
      const domain = domainInput.value.trim();
      if (!domain) {
        domainStatus.textContent = '';
        return;
      }
      if (domain.endsWith('.localhost')) {
        domainStatus.textContent = t('set.localDomain');
        return;
      }
      checkDomainBtn.disabled = true;
      domainStatus.textContent = t('set.checkingDomain');
      try {
        const res = await get('/admin/api/domain/validate?domain=' + encodeURIComponent(domain));
        domainStatus.textContent = res.message || (res.warnings || []).join(' ') || res.error || '';
        if (res.error) {
          toast.error(t('set.domainCheck', { error: res.error }));
        } else if (res.valid && res.https_available === false) {
          // Loopback/IP address: routes over plain HTTP, no HTTPS certificate.
          toast.warning(t('set.httpOnly'));
        } else if (res.valid) {
          // The inline line above already says so; no second message.
        } else {
          toast.warning(t('set.domainWarnings'));
        }
      } catch (err) {
        domainStatus.textContent = '';
        toast.error(t('set.domainCheckFailed', { error: humanizeError(err.message) }));
      }
      checkDomainBtn.disabled = readOnly;
    },
  }, t('set.check'));
  const domainRow = h('div', { style: 'display:flex; gap:8px; align-items:center;' }, [domainInput, checkDomainBtn]);

  // One-click "allow through firewall" notice, shown only when this project has
  // a public domain and the local OS firewall is actually blocking inbound.
  const firewallNotice = h('div', {});
  loadFirewallNotice(firewallNotice, project);

  // While the router forwards 80/443 here: say so, and offer to stop.
  const routerNotice = h('div', {});
  loadRouterForwardingNotice(routerNotice, project);

  // Persistent hosting-status chip: HTTPS state, failure hints, recheck, and
  // the port-conflict notice. Complements the pre-save Check button above.
  const hostingChip = createHostingChip(projectId, { scope, detail: true });

  // Model picker
  let providers = [];
  try {
    providers = await get('/admin/api/providers/catalog');
  } catch {
    // ignore
  }

  const picker = createModelPicker(providers, { currentModelId: project.llm_model_id, disabled: readOnly });
  const { providerSelect, modelSelect } = picker;

  // Banned IPs: one IP address or CIDR range per line. Stored in the project
  // config blob and enforced by the public server (403 for matching visitors).
  let initialBans = [];
  try {
    initialBans = JSON.parse(project.config || '{}').banned_ips || [];
  } catch {
    // malformed config blob: start from an empty list
  }
  const bannedIpsInput = h('textarea', {
    className: 'input',
    rows: '3',
    value: draft.bannedIps ?? initialBans.join('\n'),
    placeholder: '203.0.113.5\n198.51.100.0/24',
    disabled: readOnly,
  });

  // Per-project Content-Security-Policy override for the public site. Empty
  // means "use the instance default". Lets an owner harden one untrusted or
  // multi-user app without changing the whole instance.
  let initialCSP = '';
  try {
    initialCSP = JSON.parse(project.config || '{}').csp || '';
  } catch {
    // malformed config blob: leave empty so the instance default applies
  }
  const cspInput = h('textarea', {
    className: 'input',
    rows: '2',
    value: draft.csp ?? initialCSP,
    placeholder: "default-src 'self'; img-src 'self' data: https:; object-src 'none'",
    disabled: readOnly,
  });

  // User-flow tests (of_test.js), experimental: off unless the owner turned them on.
  // Stored in the project config blob; an absent key means off.
  let initialUserFlows = false;
  try {
    initialUserFlows = JSON.parse(project.config || '{}').user_flows === true;
  } catch {
    // malformed config blob: keep the default (off)
  }
  const userFlowsInput = h('input', { type: 'checkbox', checked: draft.userFlows ?? initialUserFlows, disabled: readOnly });
  remember('userFlows', userFlowsInput, (el) => el.checked);
  remember('bannedIps', bannedIpsInput);
  remember('csp', cspInput);
  remember('description', descInput);

  // Name, domain, description, model, banned IPs, and CSP are one atomic PUT.
  // Banned IPs / CSP live on the Advanced tab, but every Save submits the full
  // object from the inputs, which are restored from `drafts` when a section is
  // remounted, so nothing is lost regardless of which section's Save is clicked.
  async function doSave(btn) {
    nameInput.classList.remove('input--error');
    const name = nameInput.value.trim();
    if (!name) {
      toast.warning(t('projects.nameRequired'));
      nameInput.classList.add('input--error');
      return;
    }
    // The picker lists enabled models only. When the project's current model
    // was disabled, keep it so unrelated edits (domain, name) can still save.
    const selectedModel = picker.getSelectedModel();
    const modelId = selectedModel ? selectedModel.id : project.llm_model_id;
    if (!modelId) {
      toast.warning(t('set.selectModel'));
      return;
    }
    btn.disabled = true;
    const original = btn.textContent;
    btn.textContent = t('common.saving');
    try {
      const saved = await put(`/admin/api/projects/${projectId}`, {
        name,
        domain: domainInput.value.trim() || null,
        description: descInput.value.trim() || null,
        llm_model_id: modelId,
        banned_ips: bannedIpsInput.value.split('\n').map((s) => s.trim()).filter(Boolean),
        csp: cspInput.value.trim(),
        user_flows: userFlowsInput.checked,
      });
      drafts.delete(projectId);
      if (saved && saved.tls_warning) toast.warning(saved.tls_warning);
      else toast.success(t('set.saved'));
      // The domain may have changed, so every hosting surface re-derives.
      hostingChip.refresh();
      publishStrip?.refresh();
      goLiveTasks?.refresh();
    } catch (err) {
      toast.error(t('set.saveFailed', { error: humanizeError(err.message) }));
      btn.disabled = false;
      btn.textContent = original;
      return;
    }
    // The save succeeded. Refreshing the project list is a courtesy: if it
    // fails, the person must not be told the save did.
    try {
      state.set('projects', await get('/admin/api/projects'));
    } catch { /* the list refreshes on the next event */ }
    btn.disabled = false;
    btn.textContent = original;
  }

  function saveButton() {
    const btn = h('button', { className: 'btn btn--primary', onClick: () => doSave(btn) }, t('set.saveChanges'));
    return btn;
  }

  // --- General tab: the everyday fields (one Save) ---
  const generalBody = h('div', { className: 'card' }, [
    h('h4', { className: 'card__title mb-4' }, t('tabs.general')),
    formGroup(t('set.projectName'), nameInput, { required: true, hint: t('set.projectNameHint') }),
    h('div', { className: 'form-group' }, [
      h('label', {}, t('projects.col.domain')),
      domainRow,
      h('p', { className: 'form-hint' }, t('set.domainHint')),
      domainStatus,
      hostingChip,
      firewallNotice,
      routerNotice,
    ]),
    formGroup(t('rc.description'), descInput, { hint: t('set.descHint') }),
    formGroup(t('composer.aiProvider'), providerSelect, { required: true, hint: t('set.providerHint') }),
    formGroup(t('composer.aiModel'), modelSelect, { required: true, hint: t('set.modelHint') }),
    ...(state.isAdmin() ? [h('div', { className: 'mt-4' }, [saveButton()])] : []),
  ]);

  // --- Sharing & status tab: reachability and distribution (admin only) ---
  // The publish strip reconciles published/pages/domain/HTTPS into one status
  // line and carries the publish/unpublish toggle. Anything still needing a human
  // is listed by the task card below it.
  const publishStrip = state.isAdmin() ? createPublishStrip(projectId) : null;

  // Whatever is still waiting on a human - a DNS record at the registrar, a port
  // forward on the router, a look from a phone. Everything a machine can do
  // happens on its own, so this renders nothing once the list is empty.
  const goLiveTasks = state.isAdmin() ? createGoLiveTasks(projectId) : null;

  const sharingBody = h('div', {}, [
    publishStrip,
    goLiveTasks,
    buildExportCard(projectId, project),
  ].filter(Boolean));

  // --- Advanced tab: security knobs (shared Save) plus destructive actions ---
  const dangerZone = state.isAdmin() ? h('div', { className: 'danger-zone' }, [
    h('h4', { className: 'danger-zone__title' }, t('projects.delete')),
    h('p', { className: 'danger-zone__desc' }, t('set.deleteDesc')),
    h('button', {
      className: 'btn btn--danger',
      onClick: () => {
        modal.confirmByTyping(
          t('projects.delete'),
          t('projects.deleteBody', { name: project.name }),
          project.name,
          async () => {
            try {
              await del(`/admin/api/projects/${projectId}`);
              drafts.delete(projectId);
              toast.success(t('projects.deleted'));
              navigate('/projects');
            } catch (err) {
              toast.error(t('set.deleteFailed', { error: humanizeError(err.message) }));
              return false;
            }
          },
          t('projects.delete'),
        );
      },
    }, [
      h('span', { innerHTML: icon('trash') }),
      t('projects.delete'),
    ]),
  ]) : null;

  const advancedBody = h('div', {}, [
    h('div', { className: 'card' }, [
      h('h4', { className: 'card__title mb-4' }, [
        t('composer.sectionTesting'),
        h('span', { className: 'badge badge--warning', style: 'margin-inline-start: 8px;' }, t('composer.experimental')),
      ]),
      h('div', { className: 'form-group' }, [
        h('label', { className: 'toggle' }, [userFlowsInput, t('composer.flowsTitle')]),
        h('p', { className: 'form-hint' }, t('set.flowsHint')),
      ]),
      ...(state.isAdmin() ? [h('div', { className: 'mt-4' }, [saveButton()])] : []),
    ]),
    h('div', { className: 'card' }, [
      h('h4', { className: 'card__title mb-4' }, t('tabs.advanced')),
      formGroup(t('set.blocked'), bannedIpsInput, { hint: t('set.blockedHint') }),
      // Expert-only: hidden behind a disclosure so a curious owner cannot
      // break their site's scripts with one accidental edit.
      h('details', { className: 'mb-3', ...(initialCSP ? { open: true } : {}) }, [
        h('summary', { className: 'form-hint', style: { cursor: 'pointer' } }, t('set.csp')),
        formGroup('', cspInput, { hint: t('set.cspHint') }),
      ]),
      ...(state.isAdmin() ? [h('div', { className: 'mt-4' }, [saveButton()])] : []),
    ]),
    state.isAdmin() ? createSpendCard(projectId) : null,
    state.isAdmin() ? createEngineCard(projectId) : null,
    dangerZone,
  ].filter(Boolean));

  // --- Tabs (shared component, same as the Layouts page) ---
  createTabbedPanel({
    container,
    title: t('tabs.settings'),
    activeSection: section,
    defaultTab: 'general',
    tabs: [
      { id: 'general', label: t('tabs.general'), content: generalBody },
      // Sharing/status/export are admin-only; hide the whole tab for non-admins.
      ...(state.isAdmin() ? [{ id: 'sharing', label: t('set.sharingStatus'), content: sharingBody }] : []),
      { id: 'advanced', label: t('tabs.advanced'), content: advancedBody },
    ],
  });

  return () => {
    scope.dispose();
    // Moving between Settings sections remounts this panel; only drop the
    // unsaved edits when the person has actually left Settings.
    setTimeout(() => {
      if (!currentPath().startsWith(`/projects/${projectId}/settings`)) drafts.delete(projectId);
    }, 0);
  };
}

/**
 * Builds the "Export project" card. A live project can be downloaded as a
 * single .of file (a zip) so it can be shared and imported elsewhere.
 */
function buildExportCard(projectId, project) {
  const isLive = project.phase.key === 'live';

  const exportBtn = h('button', {
    className: 'btn btn--secondary',
    disabled: !isLive,
    title: isLive ? '' : t('set.exportOnlyLive'),
    onClick: async () => {
      exportBtn.disabled = true;
      const original = exportBtn.textContent;
      exportBtn.textContent = t('set.exporting');
      try {
        await download(
          `/admin/api/projects/${projectId}/export`,
          `${slugifyName(project.name)}.of`,
        );
        toast.success(t('set.exported'));
      } catch (err) {
        toast.error(t('set.exportFailed', { error: humanizeError(err.message) }));
      }
      exportBtn.disabled = !isLive;
      exportBtn.textContent = original;
    },
  }, t('set.exportTitle'));

  return h('div', { className: 'card' }, [
    h('div', { className: 'card__header' }, [
      h('h4', { className: 'card__title' }, t('set.exportTitle')),
      h('span', { innerHTML: icon('download'), className: 'card__icon' }),
    ]),
    h('p', { className: 'form-hint mt-2' },
      isLive ? t('set.exportBody') : t('set.exportOnlyLive')
    ),
    h('div', { className: 'mt-3' }, [exportBtn]),
  ]);
}

// slugifyName mirrors the backend slug used for the export filename.
function slugifyName(name) {
  const slug = (name || '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
  return slug || 'project';
}

