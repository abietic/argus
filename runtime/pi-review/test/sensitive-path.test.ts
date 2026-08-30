import assert from "node:assert/strict";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { sensitivePathReason } from "../src/git.js";
import { sensitiveInputPathReason } from "../src/sensitive-path.js";
import { loadKnowledge, loadSkills } from "../src/skills.js";

test("sensitive path policy covers environment, credential and state files", () => {
  const cases: Array<[string, string]> = [
    [".envrc", "sensitive_environment_file"],
    ["config/.dev.vars", "sensitive_environment_file"],
    ["service/.direnv/allow", "sensitive_environment_file"],
    [".vault-token", "sensitive_credential_file"],
    ["config/secret.json", "sensitive_credential_file"],
    ["config/db-secrets.yaml", "sensitive_credential_file"],
    ["config/secrets.yml", "sensitive_credential_file"],
    ["config/secret.toml", "sensitive_credential_file"],
    ["identity/service-account.json", "sensitive_credential_file"],
    ["identity/service_account.yaml", "sensitive_credential_file"],
    [
      "identity/application_default_credentials.json",
      "sensitive_credential_file",
    ],
    ["home/.kube/config", "sensitive_credential_file"],
    ["home/.terraformrc", "sensitive_credential_file"],
    ["home/.terraform.d/credentials.tfrc.json", "sensitive_credential_file"],
    ["infra/terraform.tfstate", "sensitive_state_file"],
    ["infra/terraform.tfstate.backup", "sensitive_state_file"],
    ["infra/release.tfplan", "sensitive_state_file"],
  ];

  for (const [input, expected] of cases) {
    assert.equal(sensitivePathReason(input), expected, input);
  }
});

test("example, sample and template variants remain reviewable", () => {
  const safePaths = [
    ".env.example",
    ".env.local.sample",
    ".envrc.template",
    ".dev.vars.example",
    ".vault-token.sample",
    "config/secrets.example.yaml",
    "config/db-secrets-template.json",
    "identity/service-account.sample.json",
    "identity/application_default_credentials.template.json",
    "home/.kube/config.example",
    "infra/terraform.tfstate.example",
  ];

  for (const input of safePaths) {
    assert.equal(sensitivePathReason(input), undefined, input);
  }
});

test("absolute paths use the same sensitive input policy", () => {
  assert.equal(
    sensitiveInputPathReason("/tmp/work/.direnv/environment"),
    "sensitive_environment_file",
  );
  assert.equal(
    sensitiveInputPathReason("C:\\work\\identity\\service_account.json"),
    "sensitive_credential_file",
  );
});

test("explicit skill and knowledge files reject sensitive paths", async () => {
  const directory = await mkdtemp(path.join(tmpdir(), "argus-sensitive-"));
  try {
    const skill = path.join(directory, "secrets.yaml");
    const knowledge = path.join(directory, ".dev.vars");
    await writeFile(skill, "# Do not load\n\nsecret material\n", "utf8");
    await writeFile(knowledge, "TOKEN=do-not-load\n", "utf8");

    await assert.rejects(
      loadSkills([skill]),
      /skill file is sensitive \(sensitive_credential_file\)/u,
    );
    await assert.rejects(
      loadKnowledge([knowledge]),
      /knowledge file is sensitive \(sensitive_environment_file\)/u,
    );
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("explicit template knowledge remains loadable", async () => {
  const directory = await mkdtemp(path.join(tmpdir(), "argus-template-"));
  try {
    const knowledge = path.join(directory, "secrets.example.yaml");
    await writeFile(knowledge, "example: placeholder\n", "utf8");

    const loaded = await loadKnowledge([knowledge]);
    assert.match(loaded, /example: placeholder/u);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
