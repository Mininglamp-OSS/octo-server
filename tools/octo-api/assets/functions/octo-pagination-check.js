// octo-pagination-check.js — R5 pagination contract checks.
//
// Two modes, selected via functionOptions.mode:
//
//   "shape"  — input is a (resolved) `pagination` schema object found inside
//              a 2xx response envelope. It must be one of the two R5 shapes:
//                cursor: { has_more [, next_cursor] }     — and MUST NOT
//                        carry `total` (anti-pattern: cursor 模式还返 total)
//                offset: { total, page, page_size }
//
//   "params" — input is a (resolved) operation object. For every 2xx JSON
//              response whose envelope carries `pagination`:
//                cursor shape → operation must declare a `cursor` query param
//                offset shape → operation must declare a `page` query param
//
// Returns spectral-compatible array of {message} on violation; undefined on pass.

export default function octoPaginationCheck(input, opts) {
  const mode = opts && opts.mode;
  if (mode === 'shape') return checkShape(input);
  if (mode === 'params') return checkParams(input);
}

function paginationKind(props) {
  if (!props || typeof props !== 'object') return null;
  if ('has_more' in props) return 'cursor';
  if ('total' in props && 'page' in props && 'page_size' in props) return 'offset';
  return 'unknown';
}

function checkShape(schema) {
  if (!schema || typeof schema !== 'object') return;
  const props = schema.properties;
  const kind = paginationKind(props);
  if (kind === null) return; // not an object schema we can judge
  if (kind === 'cursor' && 'total' in props) {
    return [{ message: 'cursor pagination must not include `total` — return only has_more + next_cursor (R5)' }];
  }
  if (kind === 'unknown') {
    return [{ message: 'pagination must be cursor {has_more, next_cursor} or offset {total, page, page_size} (R5)' }];
  }
}

function checkParams(op) {
  if (!op || typeof op !== 'object') return;
  const params = Array.isArray(op.parameters) ? op.parameters : [];
  const queryNames = new Set(
    params.filter((p) => p && p.in === 'query').map((p) => p.name)
  );
  const out = [];
  for (const [code, resp] of Object.entries(op.responses || {})) {
    if (!/^2/.test(code)) continue;
    const content = resp && resp.content;
    if (!content || typeof content !== 'object') continue;
    for (const media of Object.values(content)) {
      const pagination =
        media && media.schema && media.schema.properties && media.schema.properties.pagination;
      const kind = paginationKind(pagination && pagination.properties);
      if (kind === 'cursor' && !queryNames.has('cursor')) {
        out.push({ message: 'cursor-paginated response requires a `cursor` query parameter (R5)' });
      }
      if (kind === 'offset' && !queryNames.has('page')) {
        out.push({ message: 'offset-paginated response requires a `page` query parameter (R5)' });
      }
    }
  }
  if (out.length) return out;
}
