/**
 * LLM-call detail renderers for the build timeline's event-expand: system
 * prompt, messages, tool calls, and available tools.
 */

import { h } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { icon } from '../../../ui/icon.js';
import { copyText } from '../../../ui/helpers.js';
import { safeJsonParse } from '../../../core/utils.js';

function createCollapsibleSection(title, content, startCollapsed) {
  const section = h('div', { className: 'llm-log-detail__section' });
  const pre = h('pre', {}, content);

  if (startCollapsed && content.length > 500) {
    pre.style.maxHeight = '100px';
    pre.style.overflow = 'hidden';
    const toggle = h('span', {
      className: 'llm-log-detail__toggle',
      onClick: (e) => {
        e.stopPropagation();
        if (pre.style.maxHeight === '100px') {
          pre.style.maxHeight = '400px';
          pre.style.overflow = 'auto';
          toggle.textContent = t('logs.collapse');
        } else {
          pre.style.maxHeight = '100px';
          pre.style.overflow = 'hidden';
          toggle.textContent = t('logs.expand');
        }
      },
    }, t('logs.expand'));
    section.appendChild(h('div', { className: 'llm-log-detail__section-title' }, [title, toggle]));
  } else {
    section.appendChild(h('div', { className: 'llm-log-detail__section-title' }, title));
  }
  section.appendChild(pre);
  return section;
}

export function appendDetailParts(target, detail) {
  target.appendChild(h('div', { className: 'flex justify-end mb-2' }, [
    h('button', {
      className: 'btn btn--sm btn--ghost',
      onClick: (e) => {
        e.stopPropagation();
        copyText(JSON.stringify(detail, null, 2), t('logs.copied'));
      },
    }, [h('span', { innerHTML: icon('copy') }), ' ' + t('logs.copyJson')]),
  ]));

  if (detail.request_system) {
    target.appendChild(createCollapsibleSection(t('logs.systemPrompt'), detail.request_system, true));
  }

  if (detail.request_messages) {
    try {
      const msgs = JSON.parse(detail.request_messages);
      const section = h('div', { className: 'llm-log-detail__section' }, [
        h('div', { className: 'llm-log-detail__section-title' }, t('logs.messages', { count: msgs.length })),
      ]);
      msgs.forEach(msg => {
        const content = msg.content || '';
        const display = content.length > 2000 ? content.slice(0, 2000) + '\n' + t('logs.truncated') : content;
        section.appendChild(h('div', { className: `llm-log-msg llm-log-msg--${msg.role || 'user'}` }, [
          h('div', { className: 'llm-log-msg__role' }, msg.role || 'unknown'),
          h('div', { className: 'llm-log-msg__content' }, display),
        ]));
      });
      target.appendChild(section);
    } catch {
      target.appendChild(createCollapsibleSection(t('logs.messagesRaw'), detail.request_messages, false));
    }
  }

  if (detail.response_content) {
    target.appendChild(createCollapsibleSection(t('logs.response'), detail.response_content, false));
  }

  if (detail.response_tool_calls) {
    try {
      const toolCalls = JSON.parse(detail.response_tool_calls);
      if (toolCalls.length > 0) {
        const section = h('div', { className: 'llm-log-detail__section' }, [
          h('div', { className: 'llm-log-detail__section-title' }, t('logs.toolCalls', { count: toolCalls.length })),
        ]);
        toolCalls.forEach(tc => {
          let args = tc.arguments || '{}';
          const parsed = safeJsonParse(args);
          if (parsed) args = JSON.stringify(parsed, null, 2);
          section.appendChild(h('div', { className: 'llm-log-msg llm-log-msg--tool' }, [
            h('div', { className: 'llm-log-msg__role' }, `${tc.name} (${tc.id})`),
            h('pre', {}, args),
          ]));
        });
        target.appendChild(section);
      }
    } catch {
      target.appendChild(createCollapsibleSection(t('logs.toolCallsRaw'), detail.response_tool_calls, false));
    }
  }

  if (detail.request_tools) {
    const parsed = safeJsonParse(detail.request_tools);
    let body;
    if (Array.isArray(parsed)) {
      const names = parsed
        .map(tool => (typeof tool === 'string' ? tool : (tool && tool.name) || ''))
        .filter(Boolean);
      body = h('div', { className: 'text-xs text-secondary' },
        names.length ? `${names.join(', ')} (${names.length})` : t('pay.none'));
    } else {
      body = h('pre', { className: 'text-xs' }, detail.request_tools);
    }
    target.appendChild(h('div', { className: 'llm-log-detail__section' }, [
      h('div', { className: 'llm-log-detail__section-title' }, t('logs.availableTools')),
      body,
    ]));
  }
}
