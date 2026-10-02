// check-mock-rest.mjs — the e2e mock-server stands in for the backend, so its
// REST responses must be shapes the backend can produce (#2909). A field the
// mock returns and the Go response type does not declare is a test that
// passes against a server that does not exist; this finds every such field,
// at any depth, against the REST schemas' defs, and every value outside the
// enum its property declares (an EventEntry type that is not a kind).

// schemaViolations lists "path.field" for every field of value the schema
// object prop does not declare, and "path=value is not in its enum" for every
// value outside prop's enum, recursing into nested structs and arrays of
// them. A value the schema does not describe as a struct (any, map) is not
// checked.
export function schemaViolations(value, prop, defs, at) {
  const out = [];
  if (prop && prop.enum && !prop.enum.includes(value)) out.push(`${at}=${JSON.stringify(value)} is not in its enum`);
  if (!prop || value === null || typeof value !== 'object') return out;
  if (Array.isArray(value)) {
    if (!prop.items) return out;
    value.forEach((v, i) => out.push(...schemaViolations(v, prop.items, defs, `${at}[${i}]`)));
    return out;
  }
  const def = prop.$ref ? defs[prop.$ref] : prop.properties ? prop : null;
  if (!def) return out;
  for (const [k, v] of Object.entries(value)) {
    if (!(k in def.properties)) out.push(`${at}.${k}`);
    else out.push(...schemaViolations(v, def.properties[k], defs, `${at}.${k}`));
  }
  return out;
}

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
