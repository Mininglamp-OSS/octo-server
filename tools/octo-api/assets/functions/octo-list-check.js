// octo-list-check.js — Generic list-based assertion for spectral rules.
//
// Replaces several rule-specific regex strings with a declarative list +
// mode combination, so rule maintainers can edit a yaml list instead of
// crafting (or proof-reading) alternation regex.
//
// functionOptions:
//   list:    string[]            target list (required)
//   mode:    string              one of:
//     string input:
//       "forbidden"          value equals any list item → violation (blacklist)
//       "forbidden-segment"  value split by "/" has any segment in list → violation
//       "forbidden-suffix"   value ends with any list item → violation
//       "required-prefix"    value must start with one of list items
//       "required-suffix"    value must end with one of list items
//       "required-substring" value must contain one of list items
//     object input:
//       "required-key-prefix" object must have ≥1 key starting with a list item
//   message: string              custom violation message (optional)
//
// Returns spectral-compatible array of {message} when the rule is violated;
// returns undefined (no array) when the input passes the check.

const STRING_CHECKS = {
  'forbidden':          (input, list) => list.includes(input),
  'forbidden-segment':  (input, list) => input.split('/').filter(Boolean).some((s) => list.includes(s)),
  'forbidden-suffix':   (input, list) => list.some((s) => input.endsWith(s)),
  'required-prefix':    (input, list) => !list.some((p) => input.startsWith(p)),
  'required-suffix':    (input, list) => !list.some((s) => input.endsWith(s)),
  'required-substring': (input, list) => !list.some((t) => input.includes(t)),
};

const OBJECT_CHECKS = {
  'required-key-prefix': (input, list) =>
    !Object.keys(input).some((k) => list.some((p) => k.startsWith(p))),
};

export default function octoListCheck(input, opts) {
  const { list = [], mode, message } = opts || {};
  if (!Array.isArray(list) || list.length === 0 || !mode) return;

  let check;
  if (typeof input === 'string' && STRING_CHECKS[mode]) {
    check = () => STRING_CHECKS[mode](input, list);
  } else if (input !== null && typeof input === 'object' && !Array.isArray(input) && OBJECT_CHECKS[mode]) {
    check = () => OBJECT_CHECKS[mode](input, list);
  } else {
    return;
  }

  if (check()) {
    return [{
      message: message || `value "${typeof input === 'string' ? input : JSON.stringify(Object.keys(input))}" violates ${mode} list rule`,
    }];
  }
}
