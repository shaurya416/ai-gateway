/**
 * The token GET /admin/config substitutes for secret material, and the paths it
 * can appear at.
 *
 * Mirrors `RedactedSecretField` in internal/admin/repository/scrub.go. PUT
 * refuses a body where any field still holds a token, so checking the same
 * fields here lets Save explain itself before the round trip instead of after a
 * 400 — and name every field at once, where the server names only the first.
 */
export const REDACTED_PLACEHOLDER = '[REDACTED]'

/**
 * The prefix every redaction token starts with — the whole-value placeholder,
 * the narrower ones ("[REDACTED_BEARER_TOKEN]", "[REDACTED:OPENAI_API_KEY]"),
 * and the "[REDACTED_KEY_<n>]" name a withheld map entry is served under.
 *
 * A value the Admin API judged safe to serve keeps its shape and loses only its
 * secret parts, so a token arrives surrounded by the text it was cut from.
 * Matching the prefix wherever it appears mirrors `redactedMarker` on the server
 * and keeps Save refusing exactly what PUT would refuse.
 */
export const REDACTED_MARKER = '[REDACTED'

/**
 * Whether a redaction token appears anywhere in `value`, map keys included.
 *
 * Keys count because a withheld map entry comes back with its name replaced
 * too, and its value may be a `${VAR}` reference that carries no token at all:
 * `{"[REDACTED_KEY_0]": "${TOKEN}"}` is exactly what an `env` block written as
 * the documentation advises reads back as.
 */
export function containsRedactedValue(value: unknown): boolean {
  if (typeof value === 'string') return value.includes(REDACTED_MARKER)
  if (Array.isArray(value)) return value.some(containsRedactedValue)
  if (value && typeof value === 'object') {
    return Object.entries(value as Record<string, unknown>).some(
      ([key, entry]) => key.includes(REDACTED_MARKER) || containsRedactedValue(entry),
    )
  }
  return false
}

/**
 * The free-form maps of the configuration schema, by the path that reaches
 * them.
 *
 * The server reports a token anywhere inside a map as the path of the map
 * itself — the operator's unit of repair is the whole block — while a named
 * field is reported where it sits. JSON cannot tell the two apart, so the maps
 * the schema declares are listed here; a map not listed is still recognised by
 * a withheld key, which no named field can carry.
 */
const FREE_FORM_MAPS: readonly RegExp[] = [
  /^aliases$/,
  /^targets\[\d+\]\.model_map$/,
  /^plugins\[\d+\]\.config$/,
  /^mcp_servers\[\d+\]\.(headers|env)$/,
  /^observability\.tracing\.headers$/,
  /^observability\.exporters\[\d+\]\.config$/,
]

function isRecord(value: unknown): value is Record<string, unknown> {
  return value != null && typeof value === 'object' && !Array.isArray(value)
}

function collectRedacted(value: unknown, path: string, fields: string[]): void {
  if (typeof value === 'string') {
    if (value.includes(REDACTED_MARKER)) fields.push(path)
    return
  }
  if (Array.isArray(value)) {
    // A list of strings is one field, as the server reports it: a stdio
    // server's `args` comes back with every value position replaced.
    if (value.every((entry) => typeof entry === 'string')) {
      if (value.some(containsRedactedValue)) fields.push(path)
      return
    }
    value.forEach((entry, index) => collectRedacted(entry, `${path}[${index}]`, fields))
    return
  }
  if (!isRecord(value)) return
  const isMap =
    FREE_FORM_MAPS.some((pattern) => pattern.test(path))
    || Object.keys(value).some((key) => key.includes(REDACTED_MARKER))
  if (isMap) {
    if (containsRedactedValue(value)) fields.push(path)
    return
  }
  for (const [key, entry] of Object.entries(value)) {
    collectRedacted(entry, path ? `${path}.${key}` : key, fields)
  }
}

/** Every field of the document PUT would refuse, by its config path. */
export function findRedactedFields(config: Record<string, unknown>): string[] {
  const fields: string[] = []
  collectRedacted(config, '', fields)
  return fields
}

/** Whether any map in `value` serves an entry under a withheld name. */
export function hasWithheldName(value: unknown): boolean {
  if (Array.isArray(value)) return value.some(hasWithheldName)
  if (!isRecord(value)) return false
  return Object.entries(value).some(([key, entry]) => key.includes(REDACTED_MARKER) || hasWithheldName(entry))
}

/**
 * Why Save is refused, and what fixes it.
 *
 * A withheld value can be replaced in the editor. A withheld name cannot: the
 * Admin API never served it, so nothing on this page knows what it was, and the
 * advice to substitute a `${VAR}` reference would leave the placeholder name in
 * place and the save still refused.
 */
export function redactedFieldsMessage(fields: string[], withheldNames = false): string {
  const list = fields.join(', ')
  const verb = fields.length === 1 ? 'contains' : 'contain'
  const message = `${list} still ${verb} the "${REDACTED_PLACEHOLDER}" placeholder from the last read. Replace it with the real value or a \${VAR} reference before saving.`
  if (!withheldNames) return message
  return `${message} An entry named "[REDACTED_KEY_<n>]" had its name withheld too, so restore that block from the configuration file.`
}
