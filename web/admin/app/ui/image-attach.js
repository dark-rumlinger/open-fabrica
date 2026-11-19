/**
 * Shared image attachment helper for the Home prompt composer and Project Chat
 * input. Handles file picker, clipboard paste, client-side resize/compression
 * (max 1280px, JPEG 0.85), and thumbnail strip with remove buttons.
 */

import { h } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { icon } from './icon.js';

const MAX_IMAGES = 4;
const MAX_DIM = 1280;
const ALLOWED_TYPES = new Set(['image/png', 'image/jpeg', 'image/webp', 'image/gif']);

function readAsDataURL(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result || ''));
    reader.onerror = () => reject(reader.error || new Error('read failed'));
    reader.readAsDataURL(file);
  });
}

function loadImage(src) {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = () => reject(new Error('invalid image'));
    img.src = src;
  });
}

async function compressImageFile(file) {
  const rawUrl = await readAsDataURL(file);
  // Keep small GIFs/PNGs as-is if already small; resize large images on canvas.
  try {
    const img = await loadImage(rawUrl);
    const w = img.naturalWidth || img.width;
    const hPx = img.naturalHeight || img.height;
    if (!w || !hPx) return null;
    const scale = Math.min(1, MAX_DIM / Math.max(w, hPx));
    if (scale === 1 && file.size <= 512 * 1024 && ALLOWED_TYPES.has(file.type)) {
      const idx = rawUrl.indexOf(',');
      return { mime_type: file.type, data: rawUrl.slice(idx + 1), dataUrl: rawUrl };
    }
    const canvas = document.createElement('canvas');
    canvas.width = Math.max(1, Math.round(w * scale));
    canvas.height = Math.max(1, Math.round(hPx * scale));
    const ctx = canvas.getContext('2d');
    ctx.drawImage(img, 0, 0, canvas.width, canvas.height);
    const outType = file.type === 'image/png' ? 'image/png' : 'image/jpeg';
    const dataUrl = outType === 'image/png'
      ? canvas.toDataURL('image/png')
      : canvas.toDataURL('image/jpeg', 0.85);
    const idx = dataUrl.indexOf(',');
    return { mime_type: outType, data: dataUrl.slice(idx + 1), dataUrl };
  } catch {
    return null;
  }
}

export function createImageAttach({ onChange, label = '', inlinePreview = false } = {}) {
  /** @type {Array<{ mime_type: string, data: string, dataUrl: string }>} */
  const items = [];

  const fileInput = h('input', {
    type: 'file',
    accept: 'image/png,image/jpeg,image/webp,image/gif',
    multiple: !inlinePreview,
    className: 'image-attach__input',
    tabIndex: -1,
    'aria-hidden': 'true',
  });

  function buildButtonPlaceholder() {
    const children = [h('span', { className: 'image-attach__icon', innerHTML: icon('image') })];
    if (label) {
      children.push(h('span', { className: 'image-attach__label' }, label));
    }
    return children;
  }

  const button = h('button', {
    type: 'button',
    className: label ? 'image-attach__btn image-attach__btn--labeled' : 'image-attach__btn',
    title: t('chat.attachImage'),
    'aria-label': t('chat.attachImage'),
  }, buildButtonPlaceholder());

  const slot = h('div', { className: 'image-attach__slot' }, [button]);
  const strip = h('div', { className: 'image-attach__strip', hidden: true });

  function notify() {
    if (typeof onChange === 'function') onChange();
  }

  function renderInlineButton() {
    slot.querySelectorAll('.image-attach__remove').forEach((el) => el.remove());
    button.innerHTML = '';
    if (items.length === 0) {
      button.classList.remove('image-attach__btn--has-image');
      button.title = t('chat.attachImage');
      button.setAttribute('aria-label', t('chat.attachImage'));
      for (const ch of buildButtonPlaceholder()) button.appendChild(ch);
      return;
    }
    button.classList.add('image-attach__btn--has-image');
    button.title = t('chat.attachImage');
    button.setAttribute('aria-label', t('chat.attachImage'));
    button.appendChild(h('img', {
      className: 'image-attach__btn-img',
      src: items[0].dataUrl,
      alt: '',
    }));
    const removeBtn = h('button', {
      type: 'button',
      className: 'image-attach__remove',
      title: t('chat.removeImage'),
      'aria-label': t('chat.removeImage'),
    }, '×');
    removeBtn.addEventListener('click', (e) => {
      e.stopPropagation();
      items.length = 0;
      renderStrip();
      notify();
    });
    slot.appendChild(removeBtn);
  }

  function renderStrip() {
    if (inlinePreview) {
      renderInlineButton();
      return;
    }
    strip.innerHTML = '';
    strip.hidden = items.length === 0;
    items.forEach((item, idx) => {
      const removeBtn = h('button', {
        type: 'button',
        className: 'image-attach__remove',
        title: t('chat.removeImage'),
        'aria-label': t('chat.removeImage'),
      }, '×');
      removeBtn.addEventListener('click', () => {
        items.splice(idx, 1);
        renderStrip();
        notify();
      });
      strip.appendChild(h('div', { className: 'image-attach__thumb' }, [
        h('img', { src: item.dataUrl, alt: '' }),
        removeBtn,
      ]));
    });
  }

  async function addFiles(fileList) {
    const files = Array.from(fileList || []).filter((f) => f && f.type && f.type.startsWith('image/'));
    if (!files.length) return;
    if (inlinePreview) {
      const compressed = await compressImageFile(files[0]);
      if (compressed) {
        items.length = 0;
        items.push(compressed);
      }
      renderStrip();
      notify();
      return;
    }
    for (const file of files) {
      if (items.length >= MAX_IMAGES) break;
      const compressed = await compressImageFile(file);
      if (compressed && items.length < MAX_IMAGES) {
        items.push(compressed);
      }
    }
    renderStrip();
    notify();
  }

  button.addEventListener('click', () => fileInput.click());
  fileInput.addEventListener('change', () => {
    addFiles(fileInput.files);
    fileInput.value = '';
  });

  function bindPaste(el) {
    if (!el) return;
    el.addEventListener('paste', (e) => {
      const files = Array.from(e.clipboardData?.files || []).filter((f) => f.type?.startsWith('image/'));
      if (files.length > 0) {
        e.preventDefault();
        addFiles(files);
      }
    });
  }

  function bindDrop(el) {
    if (!el) return;
    el.addEventListener('dragover', (e) => {
      if (e.dataTransfer?.types?.includes('Files')) {
        e.preventDefault();
        el.classList.add('is-dragover');
      }
    });
    el.addEventListener('dragleave', () => {
      el.classList.remove('is-dragover');
    });
    el.addEventListener('drop', (e) => {
      el.classList.remove('is-dragover');
      const files = Array.from(e.dataTransfer?.files || []).filter((f) => f.type?.startsWith('image/'));
      if (files.length > 0) {
        e.preventDefault();
        addFiles(files);
      }
    });
  }

  return {
    button,
    slot,
    fileInput,
    strip,
    bindPaste,
    bindDrop,
    images() {
      return items.map(({ mime_type, data }) => ({ mime_type, data }));
    },
    previews() {
      return items.map((i) => i.dataUrl);
    },
    count() {
      return items.length;
    },
    clear() {
      items.length = 0;
      renderStrip();
      notify();
    },
    setDisabled(disabled) {
      button.disabled = !!disabled;
    },
  };
}
