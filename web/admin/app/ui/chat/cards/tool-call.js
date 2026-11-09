/**
 * Tool-call cards: the running / expandable card for each tool the brain calls,
 * plus the subtitle and result-summary extractors. Split out of message.js;
 * createToolCall is re-exported from there for the chat view.
 */

import { h } from '../../../core/dom.js';
import { t, tn } from '../../../core/i18n.js';
import { icon } from '../../icon.js';
import { disclosure } from '../../a11y.js';
import { timestamp } from '../timestamp.js';
import { createToolCard, getToolLabel } from './index.js';
/**
 * Extract a 1-line subtitle from a running tool's args so the user sees what
 * is being touched without expanding the card; "" when there is nothing
 * useful. Mirrors extractToolSummary (same shape, args vs. result).
 */
function extractRunningSubtitle(toolName, args) {
  if (!args || typeof args !== 'object') return '';
  const action = args.action || '';

  // manage_files: project_tree summarises kinds filter; everything else
  // surfaces the artifact path so the user sees which file is being touched.
  if (toolName === 'manage_files') {
    if (action === 'project_tree') {
      try {
        const kinds = args.kinds ? JSON.parse(args.kinds) : null;
        return kinds && kinds.length ? `kinds: ${kinds.join(', ')}` : '';
      } catch {
        return '';
      }
    }
    if (action === 'attach' || action === 'detach') {
      const ap = args.file_path || '';
      const pp = args.page_path || '';
      return ap && pp ? `${ap} \u2194 ${pp}` : '';
    }
    if (action === 'list' || action === 'list_page_files') {
      return args.kind || args.page_path || '';
    }
    return args.file_path || '';
  }

  // Per-tool quick subtitles. Keep these short, one path/name max.
  switch (toolName) {
    case 'manage_diagnostics':  return '';
    case 'manage_testing': {
      const target = args.endpoint_path || args.url_path || args.page_path || args.file_path || args.url || '';
      // test_module probes ONE named export/global per call, so two probes of the
      // same file with different entries otherwise render as an identical repeat.
      // Surface the entry so the card shows what was actually tested.
      if (action === 'test_module' && target && args.entry && args.entry !== 'default') {
        return `${target} · ${args.entry}`;
      }
      return target;
    }
    case 'manage_media':        return args.path || args.filename || '';
    case 'manage_data':         return args.table_name || args.table || '';
    case 'manage_schema':       return args.table_name || args.view_name || args.table || '';
    case 'manage_api':
    case 'manage_auth':
    case 'manage_relay':
    case 'manage_tick':
    case 'manage_authoritative':
    case 'manage_matchmaking':
    case 'manage_signaling':
    case 'manage_realtime':
    case 'manage_sse':
    case 'manage_llm_proxy':   return args.endpoint_path || '';
    case 'manage_chat':         return (args.message || '').slice(0, 60);
    case 'manage_memory':       return args.key || '';
    case 'manage_schedules':         return args.job_type || args.job_id || args.schedule_name || '';
    case 'manage_email':        return args.to || args.template || '';
    case 'manage_http':         return args.url || '';
    case 'manage_webhooks':     return args.path || args.url || '';
    case 'manage_payments':     return args.product_id || args.subscription_id || '';
    case 'manage_subscription_plans': return args.name || '';
    case 'manage_subscriptions':      return args.plan_id || args.subscription_id || '';
    case 'manage_keyword_search': return args.fts_query || args.index || '';
    case 'manage_vector_search':return args.semantic_query || '';
    case 'manage_secrets':      return args.name || '';
    case 'manage_notifications':return args.title || (args.body || '').slice(0, 60);
    case 'manage_analytics':    return args.endpoint_path || args.page_path || (args.start_date && args.end_date ? `${args.start_date}..${args.end_date}` : '');
    case 'manage_function':
    case 'manage_uploads':      return args.endpoint_path || args.table_name || '';
    case 'manage_import_endpoint':
    case 'manage_export_endpoint': return args.endpoint_path || args.table_name || '';
    case 'manage_feeds':        return args.endpoint_path || args.table_name || args.title || '';
    case 'manage_sql':          return args.table_name || (args.sql || '').slice(0, 60);
    case 'manage_knowledge':    return args.base || args.name || args.query || args.source || '';
    case 'manage_documents':    return args.filename || args.path || '';
    case 'manage_codebase':     return args.pattern || args.symbol || args.path_glob || '';
    case 'manage_guides':       return args.slug || args.query || '';
    case 'manage_libraries':    return (args.library && args.library.name) || args.name || '';
    case 'manage_tasks':        return (args.title || '').slice(0, 60) || args.task_id || '';
    case 'manage_triggers':     return args.name || args.trigger_type || args.event_type || '';
    case 'manage_providers':    return args.name || args.id || '';
    case 'manage_stockphotos':
    case 'manage_web_search':   return (args.query || '').slice(0, 60);
    case 'manage_image_gen':    return (args.prompt || '').slice(0, 60);
    case 'manage_push':         return args.title || args.endpoint_path || args.url || '';
    case 'manage_roles':        return args.role || args.name || args.user_id || '';
    case 'manage_tenancy':      return args.slug || args.name || args.tenant_id || args.email || '';
    case 'manage_oauth':        return args.provider_name || args.auth_path || '';
    case 'manage_2fa':          return args.auth_path || args.factor_type || '';
    case 'manage_redirects':    return args.redirect_from || args.redirect_to || '';
    case 'manage_seo':          return args.page_path || '';
  }

  // Generic last-resort: any path-shaped arg.
  return args.path || args.name || args.id || '';
}

/**
 * Extract a 1-line summary from a tool result JSON string.
 */
function extractToolSummary(result) {
  if (!result) return '';
  try {
    const parsed = typeof result === 'string' ? JSON.parse(result) : result;
    if (!parsed) return '';

    // Path-shaped result fields (any of endpoint_path / file_path / page_path / path).
    if (parsed.endpoint_path) return parsed.endpoint_path;
    if (parsed.file_path) return parsed.file_path;
    if (parsed.page_path) return parsed.page_path;
    if (parsed.path) return parsed.path;

    // Title (e.g. from tool results)
    if (parsed.title) return parsed.title;

    // File operations
    if (parsed.filename) return parsed.filename;

    // Table operations
    if (parsed.table_name) return parsed.table_name;

    // Data queries
    if (Number.isFinite(Number(parsed.rows))) return tn('chat.rowsCount', Number(parsed.rows));
    if (Array.isArray(parsed.data)) return tn('chat.resultsCount', parsed.data.length);

    // Count
    if (Number.isFinite(Number(parsed.count))) return tn('chat.itemsCount', Number(parsed.count));

    // Generic success message
    if (parsed.message) {
      const msg = parsed.message;
      return msg.length > 50 ? msg.slice(0, 47) + '...' : msg;
    }

    return '';
  } catch {
    return '';
  }
}

/**
 * Create a tool call card element.
 * Routes to interactive cards for known tool types, falls back to generic card.
 */
export function createToolCall(toolName, status, result, args) {
  // Try interactive card first
  const card = createToolCard(toolName, status, args, result);
  if (card) return card;

  // Generic fallback
  return createGenericToolCall(toolName, status, result, args);
}

// Map tool names to icons for generic tool cards
const toolIcons = {
  manage_files: 'file-text',
  manage_media: 'image',
  manage_stockphotos: 'camera',
  manage_web_search: 'search',
  manage_data: 'database',
  manage_api: 'zap',
  manage_auth: 'shield',
  manage_relay: 'chat',
  manage_tick: 'activity',
  manage_authoritative: 'shield',
  manage_matchmaking: 'users',
  manage_signaling: 'video',
  manage_realtime: 'radio',
  manage_sse: 'radio',
  manage_llm_proxy: 'radio',
  manage_chat: 'help-circle',
  manage_memory: 'book',
  manage_notifications: 'bell',
  manage_analytics: 'bar-chart',
  manage_diagnostics: 'activity',
  manage_testing: 'activity',
  manage_secrets: 'key',
  manage_providers: 'plug',
  manage_payments: 'credit-card',
  manage_subscription_plans: 'credit-card',
  manage_subscriptions: 'credit-card',
  manage_email: 'mail',
  manage_webhooks: 'globe',
  manage_http: 'globe',
  manage_project: 'settings',
  manage_schedules: 'clock',
  manage_schema: 'grid',
  manage_sql: 'database',
  manage_uploads: 'upload',
  manage_oauth: 'lock',
  manage_2fa: 'lock',
  manage_roles: 'lock',
  manage_tenancy: 'users',
  manage_keyword_search: 'search',
  manage_vector_search: 'target',
  manage_push: 'bell',
  manage_image_gen: 'image',
  manage_redirects: 'external-link',
  manage_seo: 'search',
  manage_feeds: 'rss',
  manage_import_endpoint: 'upload',
  manage_export_endpoint: 'download',
  manage_triggers: 'zap',
  manage_libraries: 'layers',
  manage_tasks: 'list',
  manage_function: 'code',
  manage_codebase: 'code',
  manage_guides: 'bookmark',
  manage_knowledge: 'bulb',
  manage_documents: 'file-text',
};

/**
 * Generic tool call card (collapsible, status badge, 1-line summary on completion).
 */
function createGenericToolCall(toolName, status, result, args) {
  const statusBadge = status === 'success'
    ? h('span', { className: 'badge badge--success' }, t('chat.done'))
    : status === 'running'
    ? h('span', { className: 'badge badge--info' }, [
        h('span', { className: 'spinner spinner--sm' }),
        ' ' + t('tasks.status.running'),
      ])
    : h('span', { className: 'badge badge--danger' }, t('chat.error'));

  const toolIcon = toolIcons[toolName] || 'settings';
  const header = h('div', { className: 'message__tool-header' }, [
    h('span', { innerHTML: icon('chevron-right'), className: 'message__tool-chevron' }),
    h('span', { innerHTML: icon(toolIcon), className: 'message__tool-icon' }),
    h('span', {}, getToolLabel(toolName, args)),
    timestamp(),
    h('span', { className: 'message__tool-status' }, [statusBadge]),
  ]);

  // Subtitle: show args context while running, then swap to the result summary
  // on completion. Without this the user sees "Running..." with no clue what's
  // being read or written until the tool returns.
  const initialSubtitle = status === 'success' && result
    ? extractToolSummary(result)
    : extractRunningSubtitle(toolName, args);
  if (initialSubtitle) {
    header.insertBefore(
      h('span', { className: 'message__tool-summary' }, initialSubtitle),
      header.querySelector('.message__tool-status')
    );
  }

  const body = h('div', { className: 'message__tool-body' });
  if (result) {
    body.textContent = typeof result === 'string' ? result : JSON.stringify(result, null, 2);
  }

  const cardEl = h('div', { className: 'message__tool-call' }, [header, body]);

  // Auto-expand while running so the user can see activity
  if (status === 'running') {
    header.classList.add('expanded');
    body.classList.add('visible');
  }

  disclosure(header, () => {
    header.classList.toggle('expanded');
    return body.classList.toggle('visible');
  }, status === 'running');

  return {
    element: cardEl,
    updateStatus(newStatus, newResult) {
      const newBadge = newStatus === 'success'
        ? h('span', { className: 'badge badge--success' }, t('chat.done'))
        : h('span', { className: 'badge badge--danger' }, t('chat.error'));
      const statusEl = header.querySelector('.message__tool-status');
      statusEl.innerHTML = '';
      statusEl.appendChild(newBadge);
      if (newResult) {
        body.textContent = typeof newResult === 'string' ? newResult : JSON.stringify(newResult, null, 2);

        // Replace the running subtitle with the result summary so the header
        // reflects the outcome, not the in-flight context. Falls back to
        // keeping the running subtitle if the result has nothing summary-shaped.
        if (newStatus === 'success') {
          const summary = extractToolSummary(newResult);
          if (summary) {
            let summaryEl = header.querySelector('.message__tool-summary');
            if (!summaryEl) {
              summaryEl = h('span', { className: 'message__tool-summary' });
              header.insertBefore(summaryEl, statusEl);
            }
            summaryEl.textContent = summary;
          }
        }
      }
      // Auto-collapse when done
      if (newStatus === 'success') {
        header.classList.remove('expanded');
        body.classList.remove('visible');
      }
    },
  };
}
