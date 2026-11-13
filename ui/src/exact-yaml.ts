import { parseDocument, type Document, type ScalarTag } from 'yaml';

// The preview uses JS numbers, while authoring must retain opaque input tokens.
// intAsBigInt covers integers; decimal tokens retain their spelling even when
// their rounded JS value selects an integer tag during serialization.
export function preserveNumericTokens(doc: Document): Document {
  doc.schema = doc.schema.clone();
  doc.schema.tags = doc.schema.tags.map(tag => {
    if (!['tag:yaml.org,2002:float', 'tag:yaml.org,2002:int'].includes(tag.tag) || !tag.stringify) return tag;
    const original = tag as ScalarTag;
    return { ...original, stringify(node, ...args) {
      if (node.source !== undefined && /^[+-]?[0-9]+$/.test(node.source) && typeof node.value === 'bigint' && BigInt(node.source) === node.value) return node.source;
      if (node.source !== undefined && /^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$/.test(node.source) && typeof node.value === 'number' && Object.is(Number(node.source), node.value)) return node.source;
      return original.stringify!(node, ...args);
    } } satisfies ScalarTag;
  });
  return doc;
}

export const parseExactYAML = (source: string) => preserveNumericTokens(parseDocument(source, { intAsBigInt: true }));
