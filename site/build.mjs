// Builds the landing page from src/page.html:
//   index.html                 the deployable site (fonts served from ./fonts)
//   <out>/zerowatch-landing.html  a single-file preview with the fonts inlined
// Usage: node site/build.mjs [previewDir]
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const page = readFileSync(join(here, "src/page.html"), "utf8");

const head = `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
`;
writeFileSync(join(here, "index.html"), head + page + "\n</html>\n");

const previewDir = process.argv[2];
if (previewDir) {
  const inlined = page.replace(/url\("fonts\/([^"]+)"\)/g, (_, f) =>
    `url("data:font/woff2;base64,${readFileSync(join(here, "fonts", f)).toString("base64")}")`);
  mkdirSync(previewDir, { recursive: true });
  writeFileSync(join(previewDir, "zerowatch-landing.html"), inlined);
}
console.log("built index.html" + (previewDir ? " and preview" : ""));
