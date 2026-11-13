// Parse-throughput comparison with acorn on the jsparse benchmark bundles
// (test-only tool; acorn is not a repository dependency).
//
//   FF_JSPARSE_BUNDLE_DIR=/tmp/b go test ./jsparse -run TestJSBundleDump
//   NODE_PATH=<scratch>/node_modules node acorn_bench.js /tmp/b
const acorn = require("acorn");
const fs = require("fs");
const path = require("path");
const dir = process.argv[2];
for (const f of fs.readdirSync(dir).filter((f) => f.endsWith(".js")).sort()) {
  const src = fs.readFileSync(path.join(dir, f), "utf8");
  const bytes = Buffer.byteLength(src);
  try {
    for (let i = 0; i < 2; i++) acorn.parse(src, { ecmaVersion: "latest" }); // warm up
  } catch (e) {
    // acorn decides regex vs division heuristically and rejects some
    // valid generated inputs (e.g. `class A extends /re/ {}`).
    console.log(`${f}\t${bytes} B\tacorn rejects: ${e.message}`);
    continue;
  }
  const runs = bytes > 5e6 ? 3 : 10;
  let best = Infinity;
  for (let i = 0; i < runs; i++) {
    const t0 = process.hrtime.bigint();
    acorn.parse(src, { ecmaVersion: "latest" });
    best = Math.min(best, Number(process.hrtime.bigint() - t0) / 1e9);
  }
  console.log(`${f}\t${bytes} B\tacorn ${(bytes / 1e6 / best).toFixed(1)} MB/s`);
}
