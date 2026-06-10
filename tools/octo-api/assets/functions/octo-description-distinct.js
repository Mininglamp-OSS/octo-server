// octo-description-distinct.js — R13 @Description anti-pattern check.
//
// Input is an operation object. The spec requires @Description to ADD
// information beyond @Summary (idempotency / side effects / defaults),
// so a description that merely repeats the summary is a violation.
// Comparison is normalized: trim, lowercase, trailing period stripped.
//
// Returns spectral-compatible array of {message} on violation; undefined on pass.

export default function octoDescriptionDistinct(input) {
  if (!input || typeof input !== 'object') return;
  const norm = (s) =>
    typeof s === 'string' ? s.trim().toLowerCase().replace(/[.。!]+$/, '') : '';
  const summary = norm(input.summary);
  const description = norm(input.description);
  if (summary && description && summary === description) {
    return [{
      message: 'description must not duplicate summary — add idempotency / side effects / defaults (R13)',
    }];
  }
}
