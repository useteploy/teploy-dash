// X01 5.2 job 3 (X02 ADR 4, "CI consumption"): schema-validate the pinned
// teploy-cli contracts corpus. This is the node leg of the dash CI job; the
// Go leg (internal/server/contracts_corpus_test.go, driven by
// TEPLOY_CONTRACTS_DIR) runs the same fixtures through dash's real decode
// paths and asserts the adoption-refusal contract for ambiguous fixtures.
//
// Usage: node validate-contracts.mjs <corpus-dir> <pinned-corpus-rev>
//
// Fails (non-zero exit) on: a schema that cannot compile, a valid/ fixture
// that does not validate, an invalid/ fixture that DOES validate, a fixture
// the expectation table does not classify (the corpus grew -- update the
// table and ci/contracts.pin consciously), or a MANIFEST revision mismatch.
// Never skips as success.
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";
import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";

const corpusDir = process.argv[2];
const pinnedCorpusRev = process.argv[3];
if (!corpusDir || !pinnedCorpusRev) {
  console.error("usage: node validate-contracts.mjs <corpus-dir> <pinned-corpus-rev>");
  process.exit(2);
}

// Expectation table: artifact -> semantics per fixture class.
//   schema:    schema/ file, ALWAYS compiled (a schema that cannot compile
//              fails the job even while its fixtures are still pending).
//   valid:     every fixture MUST validate against the schema.
//   invalid:   every fixture MUST FAIL validation (pins refusals).
//   legacy:    "schema" (must validate -- e.g. preview-state's legacy era
//              is a schema branch) or "decode-only" (pre-MI shapes that by
//              contract cannot carry machine_interface; asserted by the Go
//              decode tests, never by this schema).
//   ambiguous: adoption-refusal class (Go decode tests assert the refusal);
//              listed so a new ambiguous fixture cannot pass unclassified.
//   mode "each": the fixture is an array of examples; every ELEMENT must
//              satisfy the (string-typed) schema.
const artifacts = {
  "version-handshake": { schema: "version-handshake.schema.json", classes: { valid: "validate", invalid: "refuse" } },
  "app-list-envelope": { schema: "app-list-envelope.schema.json", classes: { valid: "validate", legacy: "decode-only" } },
  // server-status fixtures are "pending S2 tail" in the MANIFEST; the
  // producer pre-created the valid/ class dir, so it is classified now --
  // the moment fixtures land they are asserted, with no dash-side change.
  "server-status-envelope": { schema: "server-status-envelope.schema.json", classes: { valid: "validate" } },
  "error-envelope": { schema: "error-envelope.schema.json", classes: { valid: "validate", invalid: "refuse" } },
  "release-record": { schema: "release-record.schema.json", classes: { valid: "validate" } },
  "attempt-name": { schema: "attempt-name.schema.json", mode: "each", classes: { valid: "validate", invalid: "refuse" } },
  "preview-state": { schema: "preview-state.schema.json", classes: { valid: "validate", legacy: "validate", ambiguous: "adoption-refusal" } },
  // plan-record arrived with corpus rev 3 (C05's plan/apply binding). The
  // invalid class is tampered-id: schema-VALID but semantically wrong (the
  // recorded config digest no longer reproduces the plan id) - that refusal
  // is semantic, pinned by teploy-cli's apply self-consistency tests, so the
  // schema leg decodes it rather than pretending a schema can catch it.
  "plan-record": { schema: "plan-record.schema.json", classes: { valid: "validate", invalid: "decode-only" } },
  "observation-envelope": { schema: "observation-envelope.schema.json", classes: { valid: "validate" } },
  "operation-record": { schema: "operation-record.schema.json", classes: {} },
};

const failures = [];
const fail = (msg) => failures.push(msg);

// The MANIFEST's revision table is the authority: the pinned corpus rev
// must appear as a row, or dash CI is proving a revision the producer no
// longer records.
const manifest = readFileSync(join(corpusDir, "MANIFEST.md"), "utf8");
if (!new RegExp(`^\\| ${pinnedCorpusRev} \\|`, "m").test(manifest)) {
  fail(`pinned corpus rev ${pinnedCorpusRev} not found in MANIFEST.md revision table`);
}

const ajv = new Ajv2020({ allErrors: true, strict: false });
addFormats(ajv);

const validators = {};
for (const [name, spec] of Object.entries(artifacts)) {
  try {
    validators[name] = ajv.compile(JSON.parse(readFileSync(join(corpusDir, "schema", spec.schema), "utf8")));
  } catch (err) {
    fail(`schema ${name} does not compile: ${err.message}`);
  }
}

const listJson = (dir) => readdirSync(dir).filter((f) => f.endsWith(".json"));
const validateOne = (artifact, value) => {
  const v = validators[artifact];
  if (!v) return "no-validator";
  const ok = v(value);
  return ok ? "valid" : "invalid: " + v.errors.map((e) => `${e.instancePath || "(root)"} ${e.message}`).join("; ");
};

const fixturesRoot = join(corpusDir, "fixtures");
for (const artifact of readdirSync(fixturesRoot)) {
  const spec = artifacts[artifact];
  if (!spec) {
    fail(`fixtures/${artifact} is not in the expectation table (corpus grew; update ci/validate-contracts.mjs and ci/contracts.pin)`);
    continue;
  }
  const artifactDir = join(fixturesRoot, artifact);
  if (!statSync(artifactDir).isDirectory()) continue;
  for (const className of readdirSync(artifactDir)) {
    const semantics = spec.classes[className];
    if (semantics === undefined) {
      fail(`fixtures/${artifact}/${className} is not classified for this artifact`);
      continue;
    }
    if (semantics === "decode-only" || semantics === "adoption-refusal") continue; // Go decode tests own these
    const classDir = join(artifactDir, className);
    if (!statSync(classDir).isDirectory()) continue;
    for (const file of listJson(classDir)) {
      const rel = `${artifact}/${className}/${file}`;
      let data;
      try {
        data = JSON.parse(readFileSync(join(classDir, file), "utf8"));
      } catch (err) {
        fail(`${rel}: not parseable JSON: ${err.message}`);
        continue;
      }
      if (spec.mode === "each") {
        if (!Array.isArray(data)) {
          fail(`${rel}: expected an array of examples`);
          continue;
        }
        data.forEach((item, i) => {
          const verdict = validateOne(artifact, item);
          if (semantics === "validate" && verdict !== "valid") fail(`${rel}[${i}]: must validate, got ${verdict}`);
          if (semantics === "refuse" && verdict === "valid") fail(`${rel}[${i}]: must FAIL validation (pins a refusal)`);
        });
        continue;
      }
      const verdict = validateOne(artifact, data);
      if (semantics === "validate" && verdict !== "valid") fail(`${rel}: must validate, got ${verdict}`);
      if (semantics === "refuse" && verdict === "valid") fail(`${rel}: must FAIL validation (pins a refusal)`);
    }
  }
}

if (failures.length > 0) {
  console.error(`contracts corpus validation FAILED (${failures.length}):`);
  for (const f of failures) console.error("  - " + f);
  process.exit(1);
}
console.log("contracts corpus schema validation passed");
