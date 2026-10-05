import { sha256 } from "@noble/hashes/sha2.js";

// A worker keeps hashing off the UI thread and reads at most 4 MiB at a time.
self.onmessage = async ({ data: file }) => {
  try {
    const hash = sha256.create();
    const chunk = 4 * 1024 * 1024;
    for (let offset = 0; offset < file.size; offset += chunk) {
      hash.update(new Uint8Array(await file.slice(offset, offset + chunk).arrayBuffer()));
      self.postMessage({ progress: Math.min(offset + chunk, file.size) });
    }
    self.postMessage({ digest: Array.from(hash.digest(), n => n.toString(16).padStart(2, "0")).join("") });
  } catch {
    self.postMessage({ error: "Cannot read this file. Reselect it and retry." });
  }
};
