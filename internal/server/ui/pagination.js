"use strict";

export function normalizePage(response) {
  return {
    items: response?.items || [],
    nextCursor: response?.nextCursor || "",
    total: response?.total,
  };
}

// pageWindow returns the 1-based page numbers to show for numbered pagination,
// using null as an ellipsis gap, so long collections keep the first page, the
// last page, and the current page with its immediate neighbours.
export function pageWindow(currentPage, totalPages) {
  const pages = new Set([1, totalPages, currentPage]);
  for (const offset of [-1, 1]) {
    const neighbour = currentPage + offset;
    if (neighbour >= 1 && neighbour <= totalPages) {
      pages.add(neighbour);
    }
  }
  const ordered = [...pages].sort((left, right) => left - right);
  const result = [];
  let previous = 0;
  for (const page of ordered) {
    if (page - previous > 1) {
      result.push(null);
    }
    result.push(page);
    previous = page;
  }
  return result;
}

export function singleFlight(action, onBusyChange = () => {}) {
  let inFlight = false;
  return async (...arguments_) => {
    if (inFlight) {
      return false;
    }
    inFlight = true;
    onBusyChange(true);
    try {
      await action(...arguments_);
      return true;
    } finally {
      inFlight = false;
      onBusyChange(false);
    }
  };
}

// Cursor pages retain only opaque continuation tokens for back/forward
// navigation; changing a filter starts a new traversal.
export function cursorPageHistory() {
  let cursors = [""];
  return {
    cursor(index) {
      if (!Number.isInteger(index) || index < 0 || index >= cursors.length) {
        throw new RangeError("cursor page is unavailable");
      }
      return cursors[index];
    },
    record(index, nextCursor) {
      this.cursor(index);
      cursors = cursors.slice(0, index + 1);
      if (nextCursor) {
        cursors.push(nextCursor);
      }
    },
    reset() {
      cursors = [""];
    },
  };
}
