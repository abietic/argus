const TEMPLATE_MARKER = /(?:^|[._-])(?:example|sample|template)(?:$|[._-])/u;

/**
 * Classifies paths that must not be sent to a model as repository context or
 * loaded as explicit prompt material. The caller is responsible for its own
 * path-containment validation; this function deliberately accepts both
 * absolute and repository-relative paths so every input channel can share the
 * same deny policy.
 */
export function sensitiveInputPathReason(value: string): string | undefined {
  const normalized = value.replaceAll("\\", "/").toLowerCase();
  const segments = normalized.split("/").filter(Boolean);
  const basename = segments.at(-1) ?? normalized;

  // Example material is an explicit opt-in convention. Keep this check before
  // every deny rule so .env.example, secrets.sample.yaml and similar templates
  // remain reviewable.
  if (TEMPLATE_MARKER.test(basename)) return undefined;

  if (
    basename === ".env" ||
    basename.startsWith(".env.") ||
    basename === ".envrc" ||
    basename.startsWith(".envrc.") ||
    basename === ".dev.vars" ||
    basename.startsWith(".dev.vars.") ||
    segments.includes(".direnv")
  ) {
    return "sensitive_environment_file";
  }

  if (
    [
      ".netrc",
      ".npmrc",
      ".pypirc",
      ".git-credentials",
      ".vault-token",
      "credentials.json",
      "auth.json",
      "id_rsa",
      "id_ed25519",
      ".terraformrc",
      "terraform.rc",
      "credentials.tfrc.json",
    ].includes(basename) ||
    /(?:^|[._-])secrets?\.(?:json|ya?ml|toml)$/u.test(basename) ||
    /(?:^|[._-])service[-_]account(?:[._-]|$)/u.test(basename) ||
    /(?:^|[._-])application_default_credentials(?:[._-]|$)/u.test(basename) ||
    (segments.includes(".aws") && basename === "credentials") ||
    (segments.includes(".docker") && basename === "config.json") ||
    (segments.includes(".kube") && basename === "config")
  ) {
    return "sensitive_credential_file";
  }

  if (/\.(?:tfstate(?:\.backup)?|tfplan)$/u.test(basename)) {
    return "sensitive_state_file";
  }

  if (
    /\.(?:pem|key|p12|pfx|jks|keystore|kdbx|secret|secrets)$/u.test(basename)
  ) {
    return "sensitive_key_file";
  }

  return undefined;
}
