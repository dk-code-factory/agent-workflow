import { readFileSync } from 'node:fs';
import path from 'node:path';
import { parseSections, report, documentPath, specPath } from './flows-check.mjs';

// Runtime coverage: assertions in hooks alone cannot cover an empty case.
export default class FlowReporter {
  failures = [];
  sections = new Map();
  onBegin(config) {
    this.sections = parseSections(readFileSync(path.resolve(config.rootDir, '../../..', documentPath), 'utf8')).sections;
  }
  onTestEnd(test, result) {
    const match = /[/\\]flows[/\\]uf-(\d{2})\.spec\.ts$/.exec(test.location.file);
    if (!match) return;
    const inBody = step => step.category === 'expect' && !step.error || (step.category !== 'hook' && step.steps?.some(inBody));
    if (test.expectedStatus !== 'passed' || result.status !== 'passed' || !result.steps.some(inBody)) {
      const id = `UF-${match[1]}`;
      this.failures.push({ id, title: this.sections.get(id)?.title ?? test.title, file: specPath(id), testLine: test.location.line,
        missing: `passing assertions in case: ${test.title}`,
        action: 'Every UF case must pass and execute an assertion in its test body. Remove skip/fixme/fail declarations and add assertions for the documented outcome.' });
    }
  }
  onEnd() {
    if (this.failures.length) { report(this.failures, process.env.BASE); return { status: 'failed' }; }
  }
}
