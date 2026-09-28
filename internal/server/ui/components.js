"use strict";

import {api} from "./api.js";
import {cloneJSON, isExactJSONNumber, parseJSON, stringifyJSON} from "./json_codec.js";
import {requestDialogDecision} from "./dialog.js";
import {mergeFormValue} from "./form_values.js";
import {pageWindow, singleFlight} from "./pagination.js";

const notice = document.querySelector("#notice");
const noticeMessage = document.querySelector("#notice-message");
const resourceDialog = document.querySelector("#resource-dialog");
const resourceForm = document.querySelector("#resource-form");
const resourceFields = document.querySelector("#resource-fields");
const resourceTitle = document.querySelector("#resource-dialog-title");
const resourceDescription = document.querySelector("#resource-dialog-description");
const advancedEditor = document.querySelector("#advanced-editor");
const advancedJSON = document.querySelector("#advanced-json");
const formError = document.querySelector("#form-error");
const saveResource = document.querySelector("#save-resource");
const deleteResource = document.querySelector("#delete-resource");
const confirmDialog = document.querySelector("#confirm-dialog");
const confirmTitle = document.querySelector("#confirm-title");
const confirmMessage = document.querySelector("#confirm-message");
const confirmButton = document.querySelector("#confirm-action");
const secretDialog = document.querySelector("#secret-dialog");
const secretTitle = document.querySelector("#secret-title");
const secretValue = document.querySelector("#secret-value");

let formConfiguration = null;
let fieldReaders = [];
let controlSequence = 0;
let advancedActive = false;

export function element(tagName, className = "", text = null) {
  const node = document.createElement(tagName);
  if (className) {
    node.className = className;
  }
  if (text !== null && text !== undefined) {
    node.textContent = String(text);
  }
  return node;
}

export function appendChildren(parent, ...children) {
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) {
      continue;
    }
    parent.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return parent;
}

export function heading(title, description, actions = []) {
  const container = element("div", "page-heading");
  const copy = element("div");
  copy.append(element("h1", "", title));
  if (description) {
    copy.append(element("p", "", description));
  }
  container.append(copy);
  if (actions.length) {
    const actionBar = element("div", "button-row");
    actionBar.append(...actions.filter(Boolean));
    container.append(actionBar);
  }
  return container;
}

export function button(label, handler, style = "secondary") {
  const node = element("button", style, label);
  node.type = "button";
  node.addEventListener("click", (event) => {
    event.stopPropagation();
    Promise.resolve(handler(event)).catch((error) => showNotice(error.message, true));
  });
  return node;
}

export function routeLink(label, hash, className = "") {
  const link = element("a", className, label);
  link.href = hash;
  return link;
}

export function showNotice(message, isError = false) {
  noticeMessage.textContent = message;
  notice.classList.toggle("error", isError);
  notice.hidden = false;
}

export function clearNotice() {
  notice.hidden = true;
  noticeMessage.textContent = "";
}

export function badge(value) {
  const normalized = String(value ?? "unknown");
  const node = element("span", "badge", normalized);
  if (["passed", "succeeded", "delivered", "enabled", "ready", "hosted"].includes(normalized)) {
    node.classList.add("good");
  }
  if (["failed", "dead", "disabled", "blocked"].includes(normalized)) {
    node.classList.add("bad");
  }
  return node;
}

export function code(value) {
  return element("code", "", value ?? "—");
}

export function formatDate(value) {
  if (!value) {
    return "—";
  }
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? String(value) : date.toLocaleString();
}

export function formatBytes(value) {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let amount = Number(value || 0);
  let unit = 0;
  while (amount >= 1024 && unit < units.length - 1) {
    amount /= 1024;
    unit += 1;
  }
  return `${amount.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`;
}

export function renderTable(container, columns, rows, options = {}) {
  container.replaceChildren();
  if (!rows?.length) {
    container.append(element("div", "empty-state", options.empty || "No records"));
    return;
  }
  const wrapper = element("div", "table-wrap");
  const table = element("table");
  const header = element("tr");
  for (const column of columns) {
    header.append(element("th", "", column.label));
  }
  const head = element("thead");
  head.append(header);
  table.append(head);
  const body = element("tbody");
  for (const row of rows) {
    const tableRow = element("tr");
    if (options.href) {
      tableRow.tabIndex = 0;
      tableRow.dataset.selectable = "true";
      const navigate = () => window.location.assign(options.href(row));
      tableRow.addEventListener("click", navigate);
      tableRow.addEventListener("keydown", (event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          navigate();
        }
      });
    }
    for (const column of columns) {
      const cell = element("td");
      const value = column.render ? column.render(row) : row[column.key];
      appendChildren(cell, value === "" || value === null || value === undefined ? "—" : value);
      tableRow.append(cell);
    }
    body.append(tableRow);
  }
  table.append(body);
  wrapper.append(table);
  container.append(wrapper);
}

export function definitionList(values) {
  const list = element("dl", "definition-list");
  for (const [label, value] of values) {
    list.append(element("dt", "", label));
    const description = element("dd");
    appendChildren(description, value === "" || value === null || value === undefined ? "—" : value);
    list.append(description);
  }
  return list;
}

export function prettyJSON(value) {
  return stringifyJSON(value, true);
}

export function pager(loadMore, hasMore) {
  const container = element("div", "pager");
  if (hasMore) {
    let loadButton;
    const loadOnce = singleFlight(loadMore, (busy) => {
      loadButton.disabled = busy;
      loadButton.textContent = busy ? "Loading…" : "Load more";
      loadButton.setAttribute("aria-busy", String(busy));
    });
    loadButton = button("Load more", loadOnce);
    container.append(loadButton);
  } else {
    container.append(element("span", "muted", "All results loaded"));
  }
  return container;
}

// cursorPager navigates one bounded page at a time without needing a total.
export function cursorPager(index, hasNext, onSelect) {
  const container = element("div", "pager");
  let previous;
  let next;
  const select = singleFlight(onSelect, (busy) => {
    previous.disabled = busy || index === 0;
    next.disabled = busy || !hasNext;
  });
  previous = button("Previous", () => select(index - 1), "secondary small");
  next = button("Next", () => select(index + 1), "secondary small");
  previous.disabled = index === 0;
  next.disabled = !hasNext;
  container.append(previous, element("span", "muted", `Page ${index + 1}`), next);
  return container;
}

// numberedPager renders Prev / windowed page numbers / Next for an offset
// collection whose total item count is known. onSelect(page) loads that 1-based
// page; it is single-flighted so a double click cannot fire two loads.
export function numberedPager(currentPage, total, limit, onSelect) {
  const container = element("div", "pager");
  const totalPages = Math.max(1, Math.ceil((total || 0) / limit));
  if (totalPages <= 1) {
    return container;
  }
  let busy = false;
  const select = singleFlight(onSelect, (value) => {
    busy = value;
    for (const control of container.querySelectorAll("button")) {
      control.disabled = value || control.dataset.current === "true";
    }
  });
  const navButton = (label, targetPage, disabled) => {
    const control = button(label, () => select(targetPage), "secondary small");
    control.disabled = disabled || busy;
    return control;
  };
  container.append(navButton("Previous", currentPage - 1, currentPage <= 1));
  for (const entry of pageWindow(currentPage, totalPages)) {
    if (entry === null) {
      container.append(element("span", "muted", "…"));
      continue;
    }
    const isCurrent = entry === currentPage;
    const control = button(
      String(entry),
      () => select(entry),
      isCurrent ? "small" : "secondary small",
    );
    control.disabled = isCurrent || busy;
    if (isCurrent) {
      control.dataset.current = "true";
      control.setAttribute("aria-current", "page");
    }
    container.append(control);
  }
  container.append(navButton("Next", currentPage + 1, currentPage >= totalPages));
  return container;
}

export function openForm(configuration) {
  formConfiguration = configuration;
  resourceTitle.textContent = configuration.title;
  resourceDescription.textContent = configuration.description || "";
  saveResource.textContent = configuration.saveLabel || "Save";
  deleteResource.hidden = !configuration.onDelete;
  formError.hidden = true;
  advancedEditor.open = false;
  advancedActive = false;
  resourceFields.inert = false;
  buildFields(configuration.schema, configuration.value || {}, configuration.mode || "edit");
  advancedJSON.value = prettyJSON(readFields(false));
  resourceDialog.showModal();
  resourceFields.querySelector("input, select, textarea")?.focus();
}

function buildFields(schema, value, mode) {
  resourceFields.replaceChildren();
  fieldReaders = [];
  const wideTypes = ["predicates", "json", "keyvalue", "list", "checks"];
  for (const descriptor of schema) {
    if (descriptor.createOnly && mode !== "create") {
      continue;
    }
    const fieldClass = "form-field" + (wideTypes.includes(descriptor.type) ? " form-field-wide" : "");
    const field = element(descriptor.type === "checks" ? "fieldset" : "div", fieldClass);
    const label = element(descriptor.type === "checks" ? "legend" : "label", "", descriptor.label);
    const error = element("span", "field-error");
    const control = buildControl(descriptor, value[descriptor.name]);
    const controlID = `resource-control-${controlSequence += 1}`;
    const errorID = `${controlID}-error`;
    error.id = errorID;
    if (control.node.matches("input, select, textarea")) {
      control.node.id = controlID;
      label.htmlFor = controlID;
      control.node.setAttribute("aria-describedby", errorID);
      if (descriptor.required) {
        control.node.setAttribute("aria-required", "true");
      }
    } else if (descriptor.type !== "checks") {
      label.id = `${controlID}-label`;
      control.node.setAttribute("aria-labelledby", label.id);
      control.node.setAttribute("aria-describedby", errorID);
    }
    const nestedControls = control.node.matches("input, select, textarea")
      ? [control.node]
      : [...control.node.querySelectorAll("input, select, textarea")];
    for (const nestedControl of nestedControls) {
      nestedControl.setAttribute("aria-describedby", errorID);
      if (descriptor.required) {
        nestedControl.setAttribute("aria-required", "true");
      }
      if (label.id && !nestedControl.getAttribute("aria-label") && !nestedControl.closest("label")) {
        nestedControl.setAttribute("aria-labelledby", label.id);
      }
    }
    field.append(label, control.node);
    if (descriptor.help) {
      field.append(element("small", "help", descriptor.help));
    }
    field.append(error);
    resourceFields.append(field);
    fieldReaders.push({descriptor, read: control.read, error});
  }
}

function buildControl(descriptor, initialValue) {
  switch (descriptor.type) {
  case "boolean":
    return checkboxControl(initialValue, descriptor);
  case "select":
    return selectControl(descriptor.options, initialValue);
  case "number":
    return inputControl("number", initialValue ?? 0, descriptor);
  case "password":
    return inputControl("password", initialValue || "", descriptor);
  case "list":
    return listControl(initialValue || [], descriptor);
  case "keyvalue":
    return keyValueControl(initialValue || {}, descriptor);
  case "json":
    return jsonControl(initialValue ?? null);
  case "checks":
    return checksControl(descriptor.options, initialValue || []);
  case "predicates":
    return predicatesControl(initialValue || []);
  default:
    return inputControl("text", initialValue || "", descriptor);
  }
}

function inputControl(type, initialValue, descriptor) {
  const input = descriptor.multiline ? element("textarea") : element("input");
  if (!descriptor.multiline) {
    input.type = type;
  }
  input.value = initialValue;
  if (descriptor.placeholder) {
    input.placeholder = descriptor.placeholder;
  }
  if (descriptor.inputMode) {
    input.inputMode = descriptor.inputMode;
  }
  if (descriptor.min !== undefined) {
    input.min = descriptor.min;
  }
  return {node: input, read: () => type === "number" ?
    (input.value === "" ? 0 : parseJSON(input.value)) : input.value};
}

function checkboxControl(initialValue, descriptor) {
  const label = element("label", "checkbox-field");
  const input = element("input");
  input.type = "checkbox";
  input.checked = Boolean(initialValue);
  input.setAttribute("aria-label", descriptor.label);
  label.append(input, document.createTextNode(" Yes"));
  return {node: label, read: () => input.checked};
}

function selectControl(options, initialValue) {
  const select = element("select");
  for (const value of options) {
    const option = element("option", "", value);
    option.value = value;
    option.selected = value === initialValue;
    select.append(option);
  }
  return {node: select, read: () => select.value};
}

function listControl(initialValue, descriptor) {
  const container = element("div", "repeatable");
  const values = [...initialValue];
  const render = () => {
    container.replaceChildren();
    values.forEach((value, index) => {
      const row = element("div", "repeatable-row");
      const input = descriptor.multiline ? element("textarea") : element("input");
      input.setAttribute("aria-label", `${descriptor.label} row ${index + 1}`);
      input.value = value;
      input.addEventListener("input", () => { values[index] = input.value; });
      row.append(input, button("Remove", () => { values.splice(index, 1); render(); }, "text-button"));
      container.append(row);
    });
    container.append(button("Add row", () => { values.push(""); render(); }, "secondary small"));
  };
  render();
  return {node: container, read: () => values.map((value) => value.trim()).filter(Boolean)};
}

function keyValueControl(initialValue, descriptor) {
  const container = element("div", "repeatable");
  const entries = Object.entries(initialValue);
  const render = () => {
    container.replaceChildren();
    entries.forEach(([key, value], index) => {
      const row = element("div", "repeatable-row key-value-row");
      const keyInput = element("input");
      keyInput.placeholder = "Key";
      keyInput.setAttribute("aria-label", `${descriptor.label} key ${index + 1}`);
      keyInput.value = key;
      const valueInput = element("input");
      valueInput.placeholder = descriptor.valueType === "list" ? "Comma-separated values" : "Value";
      valueInput.setAttribute("aria-label", `${descriptor.label} value ${index + 1}`);
      valueInput.value = Array.isArray(value) ? value.join(", ") : value;
      keyInput.addEventListener("input", () => { entries[index][0] = keyInput.value; });
      valueInput.addEventListener("input", () => { entries[index][1] = valueInput.value; });
      row.append(keyInput, valueInput, button("Remove", () => { entries.splice(index, 1); render(); }, "text-button"));
      container.append(row);
    });
    container.append(button("Add row", () => { entries.push(["", ""]); render(); }, "secondary small"));
  };
  render();
  return {
    node: container,
    read: () => Object.fromEntries(entries.filter(([key]) => key.trim()).map(([key, value]) => [
      key.trim(),
      descriptor.valueType === "list" ? String(value).split(",").map((item) => item.trim()).filter(Boolean) : value,
    ])),
  };
}

function jsonControl(initialValue) {
  const textarea = element("textarea");
  textarea.rows = 8;
  textarea.value = prettyJSON(initialValue);
  return {
    node: textarea,
    read: () => parseJSON(textarea.value),
  };
}

function checksControl(options, initialValue) {
  const container = element("div", "checkbox-grid");
  const inputs = [];
  for (const option of options) {
    const label = element("label", "checkbox-field");
    const input = element("input");
    input.type = "checkbox";
    input.value = option;
    input.checked = initialValue.includes(option);
    inputs.push(input);
    label.append(input, document.createTextNode(option));
    container.append(label);
  }
  return {node: container, read: () => inputs.filter((input) => input.checked).map((input) => input.value)};
}

let attributePathsPromise = null;

// Loads the server's stable predicate attribute paths once and caches them.
// Falls back to an empty list so the path inputs stay usable (free text).
function attributePathOptions() {
  if (!attributePathsPromise) {
    attributePathsPromise = api.json("/api/v1")
      .then((discovery) => Array.isArray(discovery.attributePaths) ? discovery.attributePaths : [])
      .catch(() => []);
  }
  return attributePathsPromise;
}

function predicatesControl(initialValue) {
  const container = element("div", "repeatable predicate-list");
  const predicates = cloneJSON(initialValue);
  const operators = [
    "=", "!=", "<", "<=", ">", ">=", "before", "after", "matches",
    "contains", "in", "not-in", "exists", "absent",
  ];
  const listId = `predicate-paths-${controlSequence += 1}`;
  const pathList = element("datalist");
  pathList.id = listId;
  attributePathOptions().then((options) => {
    pathList.replaceChildren(...options.map((value) => {
      const option = element("option");
      option.value = value;
      return option;
    }));
  });
  const render = () => {
    container.replaceChildren();
    container.append(pathList);
    predicates.forEach((predicate, index) => {
      const row = element("div", "repeatable-row predicate-row");
      const path = element("input");
      path.placeholder = "sys.blobStore or a custom attribute path";
      path.setAttribute("list", listId);
      path.setAttribute("aria-label", `Predicate ${index + 1} attribute path`);
      path.value = predicate.path || "";
      const operator = element("select");
      operator.setAttribute("aria-label", `Predicate ${index + 1} operator`);
      for (const value of operators) {
        const option = element("option", "", value);
        option.value = value;
        option.selected = value === predicate.op;
        operator.append(option);
      }
      const literal = element("input");
      literal.placeholder = "value (JSON or text)";
      literal.setAttribute("aria-label", `Predicate ${index + 1} value`);
      literal.value = predicate.value === undefined
        ? ""
        : stringifyJSON(predicate.value);
      const remove = button("Remove", () => {
        predicates.splice(index, 1);
        render();
      }, "text-button");
      const update = () => {
        predicate.path = path.value.trim();
        predicate.op = operator.value;
        if (["exists", "absent"].includes(operator.value)) {
          delete predicate.value;
          literal.disabled = true;
        } else {
          predicate.value = parseLiteral(literal.value);
          literal.disabled = false;
        }
      };
      path.addEventListener("input", update);
      operator.addEventListener("change", update);
      literal.addEventListener("input", update);
      update();
      row.append(path, operator, literal, remove);
      container.append(row);
    });
    container.append(button("Add predicate", () => {
      predicates.push({path: "sys.lastAccessed", op: "before", value: "30d"});
      render();
    }, "secondary small"));
  };
  render();
  return {
    node: container,
    read: () => predicates.filter((predicate) => predicate.path),
  };
}

function parseLiteral(value) {
  const trimmed = value.trim();
  if (!trimmed) {
    return "";
  }
  try {
    return parseJSON(trimmed);
  } catch (_) {
    return trimmed;
  }
}

function readFields(validate = true) {
  const value = {};
  let valid = true;
  for (const field of fieldReaders) {
    field.error.textContent = "";
    try {
      const fieldValue = field.read();
      if (validate && field.descriptor.required && (
        fieldValue === "" || fieldValue === null || Array.isArray(fieldValue) && fieldValue.length === 0
      )) {
        field.error.textContent = `${field.descriptor.label} is required.`;
        valid = false;
      }
      value[field.descriptor.name] = fieldValue;
    } catch (error) {
      field.error.textContent = `Invalid value: ${error.message}`;
      valid = false;
    }
  }
  if (!valid) {
    throw new Error("Correct the highlighted fields.");
  }
  return mergeFormValue(formConfiguration?.value, value);
}

function readAdvancedJSON() {
  const value = parseJSON(advancedJSON.value);
  if (value === null || Array.isArray(value) || isExactJSONNumber(value) || typeof value !== "object") {
    throw new Error("Advanced JSON must be an object.");
  }
  return value;
}

advancedEditor.addEventListener("toggle", () => {
  if (advancedEditor.open) {
    if (advancedActive) {
      // Reopening after invalid JSON must leave the operator's text intact.
      return;
    }
    try {
      advancedJSON.value = prettyJSON(readFields(false));
    } catch (error) {
      formError.textContent = error.message;
      formError.hidden = false;
      advancedEditor.open = false;
      return;
    }
    advancedActive = true;
    resourceFields.inert = true;
    formError.hidden = true;
    return;
  }
  if (!advancedActive) {
    return;
  }
  try {
    const value = readAdvancedJSON();
    formConfiguration.value = value;
    buildFields(formConfiguration.schema, value, formConfiguration.mode || "edit");
    advancedActive = false;
    resourceFields.inert = false;
    formError.hidden = true;
  } catch (error) {
    formError.textContent = error.message;
    formError.hidden = false;
    advancedEditor.open = true;
    advancedJSON.focus();
  }
});

resourceForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (!formConfiguration) {
    return;
  }
  formError.hidden = true;
  let value;
  try {
    value = advancedActive ? readAdvancedJSON() : readFields(true);
  } catch (error) {
    formError.textContent = error.message;
    formError.hidden = false;
    return;
  }
  saveResource.disabled = true;
  try {
    await formConfiguration.onSave(value);
    closeForm();
  } catch (error) {
    formError.textContent = error.message;
    formError.hidden = false;
  } finally {
    saveResource.disabled = false;
  }
});

deleteResource.addEventListener("click", async () => {
  if (!formConfiguration?.onDelete) {
    return;
  }
  if (!await confirmAction(formConfiguration.deleteMessage || "Delete this resource?")) {
    return;
  }
  deleteResource.disabled = true;
  try {
    await formConfiguration.onDelete();
    closeForm();
  } catch (error) {
    formError.textContent = error.message;
    formError.hidden = false;
  } finally {
    deleteResource.disabled = false;
  }
});

export function closeForm() {
  if (resourceDialog.open) {
    resourceDialog.close();
  }
  formConfiguration = null;
}

export function confirmAction(message, options = {}) {
  confirmTitle.textContent = options.title || "Confirm action";
  confirmMessage.textContent = message;
  confirmButton.textContent = options.label || "Confirm";
  confirmButton.classList.toggle("danger", options.danger !== false);
  return requestDialogDecision(confirmDialog);
}

export function showSecret(title, value) {
  secretTitle.textContent = title;
  secretValue.textContent = value;
  secretDialog.showModal();
}

document.querySelector("#dismiss-notice").addEventListener("click", clearNotice);
document.querySelector("#close-resource-dialog").addEventListener("click", closeForm);
document.querySelector("#cancel-resource-dialog").addEventListener("click", closeForm);
document.querySelector("#close-secret").addEventListener("click", () => secretDialog.close());
document.querySelector("#copy-secret").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(secretValue.textContent);
    showNotice("Secret copied to the clipboard.");
  } catch (error) {
    showNotice(`Could not copy the secret: ${error.message}`, true);
  }
});

resourceDialog.addEventListener("close", () => {
  formConfiguration = null;
});
