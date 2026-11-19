/**
 * Pure folder-tree helpers for grouping a flat list of project files
 * into nested directories for the folder browser.
 */

/**
 * Normalize a file or folder path to a slash-separated relative path
 * with no leading or trailing slashes.
 *
 * @param {string} raw
 * @returns {string}
 */
export function normalizeFilePath(raw) {
  if (!raw) return '';
  return String(raw)
    .replace(/\\/g, '/')
    .split('/')
    .filter((seg) => seg !== '')
    .join('/');
}

/**
 * Join the active folder with a user-entered filename.
 * A leading slash on `name` targets root explicitly.
 *
 * @param {string} folder
 * @param {string} name
 * @returns {string}
 */
export function joinFolderPath(folder, name) {
  const trimmed = String(name || '').trim();
  if (!trimmed) return '';
  if (trimmed.startsWith('/')) return normalizeFilePath(trimmed);
  const base = normalizeFilePath(folder);
  const rel = normalizeFilePath(trimmed);
  return base ? `${base}/${rel}` : rel;
}

/**
 * Return the parent folder path, or '' when already at root.
 *
 * @param {string} folder
 * @returns {string}
 */
export function parentFolder(folder) {
  const norm = normalizeFilePath(folder);
  const idx = norm.lastIndexOf('/');
  return idx === -1 ? '' : norm.slice(0, idx);
}

/**
 * Group a flat file list into direct child folders and files for `currentFolder`.
 *
 * @param {Array<Object>} files
 * @param {string} currentFolder
 * @param {Iterable<string>} [extraFolders]
 * @returns {{ folders: Array<{ name: string, path: string, count: number }>, files: Array<Object> }}
 */
export function listFolderEntries(files, currentFolder = '', extraFolders = []) {
  const normFolder = normalizeFilePath(currentFolder);
  const prefix = normFolder ? `${normFolder}/` : '';
  const folderMap = new Map();
  const directFiles = [];

  const ensureFolder = (name) => {
    let entry = folderMap.get(name);
    if (!entry) {
      entry = { name, path: prefix + name, count: 0 };
      folderMap.set(name, entry);
    }
    return entry;
  };

  for (const file of files || []) {
    const norm = normalizeFilePath(file.filename);
    if (!norm) continue;
    if (prefix && !norm.startsWith(prefix)) continue;

    const rest = norm.slice(prefix.length);
    const slashIdx = rest.indexOf('/');
    if (slashIdx !== -1) {
      ensureFolder(rest.slice(0, slashIdx)).count += 1;
    } else if (rest) {
      directFiles.push({ ...file, displayName: rest });
    }
  }

  for (const rawFolder of extraFolders || []) {
    const norm = normalizeFilePath(rawFolder);
    if (!norm) continue;
    if (prefix && !norm.startsWith(prefix)) continue;
    const rest = norm.slice(prefix.length);
    if (!rest) continue;
    const slashIdx = rest.indexOf('/');
    ensureFolder(slashIdx === -1 ? rest : rest.slice(0, slashIdx));
  }

  const cmp = (a, b) => a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' });
  const folders = Array.from(folderMap.values()).sort((a, b) => cmp(a.name, b.name));
  directFiles.sort((a, b) => cmp(a.displayName, b.displayName));

  return { folders, files: directFiles };
}
