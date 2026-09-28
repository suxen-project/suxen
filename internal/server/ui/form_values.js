"use strict";

import {cloneJSON} from "./json_codec.js";

// A form schema describes its controls, not the complete writable API resource.
export function mergeFormValue(original, editedFields) {
  return {...cloneJSON(original || {}), ...editedFields};
}
