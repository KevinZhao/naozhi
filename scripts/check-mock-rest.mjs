// check-mock-rest.mjs — the e2e mock-server stands in for the backend, so its
// REST responses must be shapes the backend can produce (#2909). A field the
// mock returns and the Go response type does not declare is a test that
// passes against a server that does not exist; this finds every such field,
// at any depth, against the REST schemas' defs, and every value outside the
// enum its property declares (an EventEntry type that is not a kind).

// schemaViolations lists "path.field" for every field of value the schema
// object prop does not declare and "path=value is not in its enum" for every
// value outside prop's enum, recursing into nested structs and arrays of
// them. A value the schema does not describe (any, map) is not checked.
// With opts.strict it also lists "path.field is required" for every key a def
// requires and value lacks, and "path is not a T" for a value of the wrong
// JSON type (a string time, a null or a string where an entry belongs; null
// passes as an array, which is how Go sends a nil slice). The event routes
// are strict; the REST mock's /api/sessions still omits keys the Go type
// always sends, so its check is not.
export function schemaViolations(value, prop, defs, at, opts = {}) {
  const out = [];
  if (!prop) return out;
  if (prop.enum && !prop.enum.includes(value)) out.push(`${at}=${JSON.stringify(value)} is not in its enum`);
  const want = prop.type;
  if (opts.strict && want in JSON_TYPES && !JSON_TYPES[want](value)) out.push(`${at} is not ${/^[aio]/.test(want) ? 'an' : 'a'} ${want}`);
  if (value === null || typeof value !== 'object') return out;
  if (Array.isArray(value)) {
    if (!prop.items) return out;
    value.forEach((v, i) => out.push(...schemaViolations(v, prop.items, defs, `${at}[${i}]`, opts)));
    return out;
  }
  const def = prop.$ref ? defs[prop.$ref] : prop.properties ? prop : null;
  if (!def) return out;
  if (opts.strict) for (const k of def.required || []) if (!(k in value)) out.push(`${at}.${k} is required`);
  for (const [k, v] of Object.entries(value)) {
    if (!(k in def.properties)) out.push(`${at}.${k}`);
    else out.push(...schemaViolations(v, def.properties[k], defs, `${at}.${k}`, opts));
  }
  return out;
}

// JSON_TYPES checks a value against the JSON type its schema property names;
// a type not listed here ("any") takes every value.
const JSON_TYPES = {
  string: (v) => typeof v === 'string',
  integer: (v) => Number.isInteger(v),
  number: (v) => typeof v === 'number',
  boolean: (v) => typeof v === 'boolean',
  object: (v) => v !== null && typeof v === 'object' && !Array.isArray(v),
  array: (v) => v === null || Array.isArray(v),
};

// EVENT_ENTRIES describes a JSON array of EventEntry: the body of the three
// event routes, checked against wsproto.schema.json's defs, whose EventEntry
// def is the wire view (no linkage fields, type closed over the kinds).
export const EVENT_ENTRIES = { type: 'array', items: { type: 'object', $ref: 'clievent.EventEntry' } };

// unionResponse merges several response variants of one route into the one
// object a response may take the fields of: the first variant that declares
// a field describes it (list the typed variant first).
export function unionResponse(...variants) {
  const properties = {};
  for (const v of variants) {
    for (const [k, p] of Object.entries(v.properties)) if (!(k in properties)) properties[k] = p;
  }
  return { properties };
}
