"use strict";

export function navigateOrRefresh(targetHash, options = {}) {
  const currentHash = options.currentHash ?? window.location.hash;
  const navigate = options.navigate ?? ((hash) => window.location.assign(hash));
  const refresh = options.refresh ?? refreshCurrentRoute;
  if (currentHash === targetHash) {
    refresh();
    return "refreshed";
  }
  navigate(targetHash);
  return "navigated";
}

export function refreshCurrentRoute() {
  window.dispatchEvent(new CustomEvent("suxen:resource-changed"));
}
