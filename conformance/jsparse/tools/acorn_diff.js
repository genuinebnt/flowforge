// Differential check of jsparse against acorn (test-only tool; acorn is
// not a dependency of the repository).
//
//   npm install acorn            # in a scratch directory
//   NODE_PATH=<scratch>/node_modules node acorn_diff.js list.txt dump.tsv
//
// list.txt holds one source path per line ("module:" prefix for modules).
// dump.tsv is produced by:
//   FF_JSPARSE_DUMP=list.txt FF_JSPARSE_DUMP_OUT=dump.tsv \
//     go test ./jsparse -run TestJSOracleDump
// The script prints every file where the two parsers disagree on
// acceptance or on the canonical AST (sorted keys, no positions, no raw,
// regex/bigint values as null — the jsparse canonical JSON schema).
const acorn = require("acorn");
const fs = require("fs");

function canon(n) {
  if (Array.isArray(n)) return n.map(canon);
  if (n === null || typeof n !== "object") return n;
  const o = {};
  for (const k of Object.keys(n).sort()) {
    if (k === "start" || k === "end" || k === "loc" || (k === "raw" && n.type === "Literal")) continue;
    let v = n[k];
    if (k === "value" && n.type === "Literal" && (n.regex || n.bigint !== undefined)) v = null;
    if (typeof v === "number" && !isFinite(v)) v = null;
    o[k] = canon(v);
  }
  return o;
}

const [listFile, dumpFile] = process.argv.slice(2);
const ours = new Map();
for (const line of fs.readFileSync(dumpFile, "utf8").split("\n")) {
  if (!line) continue;
  const i = line.indexOf("\t");
  ours.set(line.slice(0, i), line.slice(i + 1));
}
let diffs = 0, total = 0;
for (let line of fs.readFileSync(listFile, "utf8").split("\n")) {
  if (!line) continue;
  let sourceType = "script";
  if (line.startsWith("module:")) { line = line.slice(7); sourceType = "module"; }
  total++;
  let theirs;
  try {
    theirs = JSON.stringify(canon(acorn.parse(fs.readFileSync(line, "utf8"), { ecmaVersion: "latest", sourceType, allowHashBang: true })));
  } catch (e) {
    theirs = "ERROR " + e.message;
  }
  const mine = ours.get(line) || "MISSING";
  const bothErr = mine.startsWith("ERROR") && theirs.startsWith("ERROR");
  if (!bothErr && mine !== theirs) {
    diffs++;
    console.log("DIFF " + line + "\n  jsparse: " + mine.slice(0, 300) + "\n  acorn:   " + theirs.slice(0, 300));
  }
}
console.log(`${diffs} of ${total} files differ`);
