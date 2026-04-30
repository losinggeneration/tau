// Plain-JS bootstrap so `node index.js` works without a build step:
// node has no native TypeScript support, so we hand off to jiti (already a
// runtime dependency for loading pi extensions) to import the real TS entry.
//
// Everything below this file is TypeScript.

import { createJiti } from "jiti";

const jiti = createJiti(import.meta.url, { interopDefault: true });
await jiti.import("./src/index.ts", { default: true });
