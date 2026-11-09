/**
 * Minimal markdown-to-HTML renderer. Safety invariant: untrusted input is
 * HTML-escaped FIRST, then markdown syntax is replaced with fixed tags;
 * attribute values (href/src/class) come only from validated URL schemes
 * (http/https/relative, data:image/* for images) or \w* captures, so neither
 * attribute breakout nor javascript: URLs can slip in.
 */

import { escapeHtml } from './utils.js';
import { appPath } from './router.js';

/**
 * Render markdown text to an HTML string.
 */
export function render(text) {
  if (!text) return '';

  // Normalize line endings. NUL is stripped because it delimits the placeholders
  // below, so input can never forge one.
  let md = text.replace(/\r\n/g, '\n').replace(/\u0000/g, '');

  // Code is set aside behind placeholders so the markdown passes below (headers,
  // bold, lists, links) cannot rewrite what is inside it, e.g. "# comment" or
  // "**kwargs". The real HTML goes back in at the very end.
  const held = [];
  const hold = (html, block) => {
    held.push(html);
    return `\u0000${block ? 'B' : 'I'}${held.length - 1}\u0000`;
  };

  // Escape HTML, this is the single point where untrusted chars become inert.
  md = escapeHtml(md);

  // Code blocks (``` ... ```)
  md = md.replace(/```(\w*)\n([\s\S]*?)```/g, (_, lang, code) => {
    return hold(`<pre><code class="lang-${lang}">${code.trim()}</code></pre>`, true);
  });

  // Inline code
  md = md.replace(/`([^`]+)`/g, (_, code) => hold(`<code>${code}</code>`, false));

  // Headers
  md = md.replace(/^#### (.+)$/gm, '<h4>$1</h4>');
  md = md.replace(/^### (.+)$/gm, '<h3>$1</h3>');
  md = md.replace(/^## (.+)$/gm, '<h2>$1</h2>');
  md = md.replace(/^# (.+)$/gm, '<h1>$1</h1>');

  // Bold + italic
  md = md.replace(/\*\*\*(.+?)\*\*\*/g, '<strong><em>$1</em></strong>');
  // Bold
  md = md.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');
  // Italic
  md = md.replace(/\*(.+?)\*/g, '<em>$1</em>');

  // Images: allow http(s), same-origin absolute paths, and data:image/* URLs.
  // MUST run BEFORE the link regex, the link pattern also matches `![…](…)`
  // because its `[` is not anchored. Reject every other scheme for the same
  // reasons as links (no javascript:, no vbscript:, no protocol-relative).
  md = md.replace(/!\[([^\]]*)\]\(([^)]+)\)/g, (_, alt, url) => {
    const safe =
      /^https?:\/\//i.test(url) ||
      /^data:image\/(png|jpeg|jpg|gif|webp|svg\+xml);/i.test(url) ||
      (url.startsWith('/') && !url.startsWith('//') && !url.startsWith('/\\'));
    if (safe) {
      return `<img src="${url}" alt="${alt}" class="markdown-image" loading="lazy">`;
    }
    return alt;
  });

  // Links: allow only http(s), same-origin absolute paths, and in-page anchors.
  // Reject javascript:, data:, vbscript:, file: and protocol-relative (//host).
  md = md.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_, text, url) => {
    const safe =
      /^https?:\/\//i.test(url) ||
      (url.startsWith('/') && !url.startsWith('//') && !url.startsWith('/\\')) ||
      url.startsWith('#');
    const route = appPath(url);
    if (route) {
      return `<a href="${route}" data-route>${text}</a>`;
    }
    if (safe) {
      return `<a href="${url}" target="_blank" rel="noopener noreferrer">${text}</a>`;
    }
    return text;
  });

  // Unordered lists
  md = md.replace(/^(\s*)[-*] (.+)$/gm, (_, indent, content) => {
    return `${indent}<li>${content}</li>`;
  });
  // Wrap consecutive <li> in <ul>
  md = md.replace(/((?:<li>.*<\/li>\n?)+)/g, '<ul>$1</ul>');

  // Ordered lists
  md = md.replace(/^(\s*)\d+\. (.+)$/gm, (_, indent, content) => {
    return `${indent}<oli>${content}</oli>`;
  });
  md = md.replace(/((?:<oli>.*<\/oli>\n?)+)/g, (match) => {
    return '<ol>' + match.replace(/<\/?oli>/g, (t) => t.replace('oli', 'li')) + '</ol>';
  });

  // Horizontal rules
  md = md.replace(/^---$/gm, '<hr>');

  // Paragraphs: wrap remaining lines that are not already wrapped in block tags
  const lines = md.split('\n');
  const result = [];
  let inParagraph = false;

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const trimmed = line.trim();

    if (!trimmed) {
      if (inParagraph) {
        result.push('</p>');
        inParagraph = false;
      }
      continue;
    }

    const isBlock = /^<(h[1-6]|ul|ol|li|pre|hr|blockquote)/.test(trimmed) || trimmed.startsWith('\u0000B');
    if (isBlock) {
      if (inParagraph) {
        result.push('</p>');
        inParagraph = false;
      }
      result.push(line);
    } else {
      if (!inParagraph) {
        result.push('<p>');
        inParagraph = true;
      }
      result.push(line);
    }
  }

  if (inParagraph) {
    result.push('</p>');
  }

  return result.join('\n').replace(/\u0000[BI](\d+)\u0000/g, (_, i) => held[Number(i)]);
}
