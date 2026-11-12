const DRAFT_LANGUAGE = /\b(candidate|proposed|unconfirmed|unverified|unresolved|TODO|FIXME)\b|must be checked/i;

module.exports = function noDraftLanguage(value) {
  if (typeof value !== 'string' || !DRAFT_LANGUAGE.test(value)) return;
  return [{ message: 'Description contains provisional contract-review language' }];
};
