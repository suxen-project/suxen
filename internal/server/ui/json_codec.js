"use strict";

// JSON numbers can exceed JavaScript's range and precision. Keep those tokens
// opaque until they are sent back to the API. The brand is private: an ordinary
// response object with the same properties can never become a raw JSON number.
const exactNumbers = new WeakSet();

function exactNumber(token) {
  const value = Object.freeze({
    toString: () => token,
    valueOf: () => Number(token),
    [Symbol.toPrimitive]: (hint) => hint === "string" ? token : Number(token),
  });
  exactNumbers.add(value);
  return value;
}

export function isExactJSONNumber(value) {
  return value !== null && typeof value === "object" && exactNumbers.has(value);
}

function numberValue(token) {
  // Fractional and exponent spellings retain their exact decimal value and
  // spelling. Integers within the safe range are useful as native UI counters.
  if (token === "-0" || token.includes(".") || /e/i.test(token) || !Number.isSafeInteger(Number(token))) {
    return exactNumber(token);
  }
  return Number(token);
}

export function parseJSON(source) {
  const input = String(source);
  let offset = 0;
  const fail = () => { throw new SyntaxError(`Invalid JSON at position ${offset}`); };
  const space = () => { while (offset < input.length && /[ \t\r\n]/.test(input[offset])) offset += 1; };
  const string = () => {
    const start = offset++;
    while (offset < input.length) {
      if (input[offset] === '"') {
        offset += 1;
        return JSON.parse(input.slice(start, offset));
      }
      if (input[offset] === "\\") offset += 1;
      offset += 1;
    }
    fail();
  };
  const value = () => {
    space();
    const char = input[offset];
    if (char === '"') return string();
    if (char === "{") {
      offset += 1;
      const result = {};
      space();
      if (input[offset] === "}") { offset += 1; return result; }
      while (true) {
        if (input[offset] !== '"') fail();
        const key = string();
        space();
        if (input[offset++] !== ":") fail();
        const entry = value();
        Object.defineProperty(result, key, {value: entry, enumerable: true, writable: true, configurable: true});
        space();
        const separator = input[offset++];
        if (separator === "}") return result;
        if (separator !== ",") fail();
        space();
      }
    }
    if (char === "[") {
      offset += 1;
      const result = [];
      space();
      if (input[offset] === "]") { offset += 1; return result; }
      while (true) {
        result.push(value());
        space();
        const separator = input[offset++];
        if (separator === "]") return result;
        if (separator !== ",") fail();
      }
    }
    for (const [word, parsed] of [["true", true], ["false", false], ["null", null]]) {
      if (input.startsWith(word, offset)) { offset += word.length; return parsed; }
    }
    const match = input.slice(offset).match(/^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/);
    if (match) { offset += match[0].length; return numberValue(match[0]); }
    fail();
  };
  const parsed = value();
  space();
  if (offset !== input.length) fail();
  return parsed;
}

export function stringifyJSON(value, pretty = false) {
  const active = new Set();
  const indent = pretty ? "  " : "";
  const serialize = (item, depth) => {
    if (isExactJSONNumber(item)) return item.toString();
    if (item === null || typeof item !== "object") {
      if (typeof item === "number" && !Number.isFinite(item)) {
        throw new TypeError("Non-finite number cannot be serialized as JSON");
      }
      if (typeof item === "bigint") throw new TypeError("BigInt cannot be serialized as JSON");
      return JSON.stringify(item);
    }
    if (active.has(item)) throw new TypeError("Circular JSON value");
    active.add(item);
    const entries = Array.isArray(item)
      ? Array.from({length: item.length}, (_, index) => [null, item[index]])
      : Object.entries(item);
    const parts = [];
    for (const [key, entry] of entries) {
      const encoded = serialize(entry, depth + 1);
      if (encoded === undefined && key !== null) continue;
      const prefix = key === null ? "" : `${JSON.stringify(key)}:${pretty ? " " : ""}`;
      parts.push(`${prefix}${encoded === undefined ? "null" : encoded}`);
    }
    active.delete(item);
    const open = Array.isArray(item) ? "[" : "{";
    const close = Array.isArray(item) ? "]" : "}";
    if (!pretty || parts.length === 0) return `${open}${parts.join(",")}${close}`;
    return `${open}\n${parts.map((part) => `${indent.repeat(depth + 1)}${part}`).join(",\n")}\n${indent.repeat(depth)}${close}`;
  };
  return serialize(value, 0);
}

export function cloneJSON(value) {
  if (isExactJSONNumber(value) || value === null || typeof value !== "object") return value;
  if (Array.isArray(value)) return value.map(cloneJSON);
  return Object.fromEntries(Object.entries(value).map(([key, entry]) => [key, cloneJSON(entry)]));
}
