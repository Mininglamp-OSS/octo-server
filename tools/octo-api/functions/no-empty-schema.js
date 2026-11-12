module.exports = function noEmptySchema(value) {
  const violations = [];

  function walk(node, path) {
    if (Array.isArray(node)) {
      node.forEach((item, index) => walk(item, `${path}[${index}]`));
      return;
    }
    if (node === null || typeof node !== 'object') return;
    if (typeof node.$ref === 'string') return;

    const types = Array.isArray(node.type) ? node.type : [node.type];
    if (types.includes('object')) {
      const properties = node.properties && typeof node.properties === 'object'
        ? Object.keys(node.properties)
        : [];
      const intentionalEmpty = node.maxProperties === 0 && node.additionalProperties === false;
      const explicitlyOpen = node.additionalProperties === true ||
        (node.additionalProperties !== null && typeof node.additionalProperties === 'object');
      if (properties.length === 0 && !intentionalEmpty && !explicitlyOpen) {
        violations.push({ message: `${path} defines an object without properties; add concrete properties, explicitly allow additionalProperties, or use the EmptyData shape` });
      }
    }
    if (types.includes('array') && !node.items) {
      violations.push({ message: `${path} defines an array without an item schema` });
    }

    if (node.properties && typeof node.properties === 'object') {
      for (const [name, property] of Object.entries(node.properties)) {
        walk(property, `${path}.properties.${name}`);
      }
    }
    if (node.items) walk(node.items, `${path}.items`);
    if (node.additionalProperties && typeof node.additionalProperties === 'object') {
      walk(node.additionalProperties, `${path}.additionalProperties`);
    }
    for (const keyword of ['allOf', 'anyOf', 'oneOf']) {
      if (Array.isArray(node[keyword])) {
        node[keyword].forEach((schema, index) => walk(schema, `${path}.${keyword}[${index}]`));
      }
    }
  }

  for (const [name, schema] of Object.entries(value || {})) {
    walk(schema, `components.schemas.${name}`);
  }
  return violations.length ? violations : undefined;
}
