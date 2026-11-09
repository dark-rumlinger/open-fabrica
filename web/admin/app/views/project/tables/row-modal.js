/**
 * Row add/edit/delete modals for the dynamic-tables view. The form-build and
 * form-collect helpers are shared so add and edit stay in sync.
 */

import { h } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { post, put, del } from '../../../core/http.js';
import * as toast from '../../../ui/toast.js';
import * as modal from '../../../ui/modal.js';
import { runAction } from '../../../ui/actions.js';
import { createFormModal } from '../../../ui/form-modal.js';
import { formGroup } from '../../../ui/helpers.js';
import { safeJsonParse } from '../../../core/utils.js';

function parseSchema(raw) {
  if (typeof raw === 'object' && raw !== null) return raw;
  return safeJsonParse(raw, {});
}

// Schema entries can be either a string ("TEXT") or an object
// ({"type":"TEXT","not_null":true,...}). Always read through this helper.
export function columnType(def) {
  if (def && typeof def === 'object') return String(def.type || '').toUpperCase();
  return String(def || '').toUpperCase();
}

function buildRowForm(schema, secureCols, existingData) {
  const fields = [];
  if (!schema) return fields;
  for (const [colName, colType] of Object.entries(schema)) {
    const isPassword = secureCols?.[colName] === 'hash';
    const isEncrypted = secureCols?.[colName] === 'encrypt';
    const upper = columnType(colType);

    let input;
    if (upper === 'BOOLEAN') {
      input = h('input', { className: 'input', type: 'checkbox', name: colName });
      if (existingData) input.checked = !!existingData[colName];
    } else {
      let inputType = 'text';
      if (upper === 'INTEGER' || upper === 'REAL') inputType = 'number';
      if (isPassword) inputType = 'password';
      const currentValue = existingData ? existingData[colName] : '';
      const asText = currentValue != null ? String(currentValue) : '';
      // Long or multi-line text needs room; short values stay a one-line field.
      const multiline = inputType === 'text' && !isPassword && !isEncrypted
        && (asText.length > 80 || asText.includes('\n'));
      // Suppress browser autofill on every field. A masked PASSWORD input sitting
      // next to a username-like column (e.g. email) is a credential-manager
      // autofill target: it would fill the saved login silently, and saving the
      // row then overwrites the stored secret with a value the admin never saw.
      // "new-password" is the signal browsers honor to skip filling a current
      // credential; "off" keeps the paired text fields from acting as the username.
      input = h(multiline ? 'textarea' : 'input', {
        className: 'input',
        ...(multiline ? { rows: '4' } : { type: inputType }),
        name: colName,
        autocomplete: isPassword ? 'new-password' : 'off',
        value: (isPassword || isEncrypted) ? '' : asText,
        placeholder: (isPassword || isEncrypted)
          ? (existingData ? t('tbl.keepCurrent') : '')
          : '',
      });
    }

    let label = colName;
    if (isPassword) label += ' ' + t('tbl.passwordSuffix');
    if (isEncrypted) label += ' ' + t('tbl.encryptedSuffix');

    if (upper === 'BOOLEAN') {
      fields.push(h('div', { className: 'form-group' }, [
        h('label', { className: 'toggle' }, [input, label]),
      ]));
    } else {
      fields.push(formGroup(label, input));
    }
  }
  return fields;
}

// With `original` (an edit), only fields that differ are returned, and a field
// the person emptied is sent as null so a value can actually be cleared. Without
// it (an add), blank fields are simply left out.
function collectFormData(form, schema, secureCols, original) {
  const data = {};
  if (!schema) return data;
  for (const [colName, colType] of Object.entries(schema)) {
    const isPassword = secureCols?.[colName] === 'hash';
    const isEncrypted = secureCols?.[colName] === 'encrypt';
    const upper = columnType(colType);
    const input = form.querySelector(`[name="${colName}"]`);
    if (!input) continue;

    if (upper === 'BOOLEAN') {
      const next = input.checked ? 1 : 0;
      if (!original || next !== (original[colName] ? 1 : 0)) data[colName] = next;
    } else if (isPassword || isEncrypted) {
      // Never pre-filled, so blank always means "keep the stored value".
      if (input.value !== '') data[colName] = input.value;
    } else if (original) {
      const before = original[colName] != null ? String(original[colName]) : '';
      if (input.value === before) continue;
      if (input.value === '') data[colName] = null;
      else data[colName] = (upper === 'INTEGER' || upper === 'REAL') ? Number(input.value) : input.value;
    } else if (input.value !== '') {
      data[colName] = (upper === 'INTEGER' || upper === 'REAL')
        ? Number(input.value) : input.value;
    }
  }
  return data;
}

export function showAddRowModal(projectId, table, reload) {
  const schema = parseSchema(table.schema_def);
  const secureCols = parseSchema(table.secure_columns);
  const form = h('div', {}, buildRowForm(schema, secureCols, null));

  createFormModal({
    title: t('tbl.addRow'),
    fields: [form],
    submitLabel: t('tbl.add'),
    validate: () => (Object.keys(collectFormData(form, schema, secureCols)).length === 0
      ? t('tbl.oneField') : null),
    submit: () => post(`/admin/api/projects/${projectId}/tables/${table.table_name}/rows`, {
      data: collectFormData(form, schema, secureCols),
    }),
    success: t('tbl.rowAdded'),
    onDone: reload,
  });
}

export function showEditRowModal(projectId, table, row, responseData, reload) {
  const schema = responseData.schema || parseSchema(table.schema_def);
  const secureCols = responseData.secure_columns || parseSchema(table.secure_columns);
  const form = h('div', {}, buildRowForm(schema, secureCols, row));

  createFormModal({
    title: t('tbl.editRow'),
    fields: [form],
    validate: () => (Object.keys(collectFormData(form, schema, secureCols, row)).length === 0
      ? t('tasks.noChanges') : null),
    submit: () => put(`/admin/api/projects/${projectId}/tables/${table.table_name}/rows/${row.id}`, {
      data: collectFormData(form, schema, secureCols, row),
    }),
    success: t('tbl.rowUpdated'),
    onDone: reload,
  });
}

export function confirmDeleteRow(projectId, table, row, reload) {
  modal.confirmDanger(t('tbl.deleteRow'), t('tbl.deleteRowBody', { id: row.id }),
    () => runAction(() => del(`/admin/api/projects/${projectId}/tables/${table.table_name}/rows/${row.id}`), {
      success: t('tbl.rowDeleted'),
      onDone: reload,
    }));
}

export function showAddColumnModal(projectId, table, onDone) {
  const nameInput = h('input', { className: 'input', placeholder: 'column_name' });
  const typeSelect = h('select', { className: 'input' }, [
    h('option', { value: 'TEXT' }, 'TEXT'),
    h('option', { value: 'INTEGER' }, 'INTEGER'),
    h('option', { value: 'REAL' }, 'REAL'),
    h('option', { value: 'BOOLEAN' }, 'BOOLEAN'),
    h('option', { value: 'PASSWORD' }, 'PASSWORD (bcrypt)'),
    h('option', { value: 'ENCRYPTED' }, 'ENCRYPTED (AES)'),
  ]);
  const notNullCheck = h('input', { type: 'checkbox' });

  createFormModal({
    title: t('tbl.addColumnTo', { table: table.table_name }),
    fields: [
      formGroup(t('tbl.columnName'), nameInput),
      formGroup(t('tbl.type'), typeSelect),
      h('div', { className: 'form-group' }, [h('label', { className: 'toggle' }, [notNullCheck, 'NOT NULL'])]),
    ],
    submitLabel: t('tbl.addColumn'),
    validate: () => (!nameInput.value.trim() ? t('tbl.columnRequired') : null),
    submit: () => post(`/admin/api/projects/${projectId}/tables/${table.table_name}/columns`, {
      column_name: nameInput.value.trim(),
      column_type: typeSelect.value,
      not_null: notNullCheck.checked,
    }),
    // The success toast names the column, so it can't be a static `success`
    // string; onDone raises it after the create lands.
    onDone: () => {
      toast.success(t('tbl.columnAdded', { name: nameInput.value.trim() }));
      onDone();
    },
  });
}
