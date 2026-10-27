export type Path = (string | number)[];
export type Mapping = Record<string, unknown>;
export interface Step extends Mapping {
  id?: string; name?: string; uses?: string; argv?: string[]; run?: string; shell?: string;
  with?: Mapping; if?: string; 'continue-on-error'?: boolean;
}
export interface Job extends Mapping {
  name?: string; needs?: string | string[]; uses?: string; steps?: Step[]; outputs?: Record<string, string>;
  repeat?: { 'max-attempts': number; until: string } | null; if?: string;
}
export interface Workflow extends Mapping { name?: string; on?: Mapping; jobs: Record<string, Job> }
export interface Diagnostic {
  severity: 'error' | 'warning' | 'info'; code: string; message: string; path: Path;
  location?: { line: number; column: number; offset: number; endOffset: number };
  source: 'parser' | 'browser' | 'host';
}
export interface Snapshot {
  source: string; workflow?: Workflow; previewWorkflow?: Workflow; diagnostics: Diagnostic[]; stale: boolean;
  revision: number; canUndo: boolean; canRedo: boolean; validating: boolean;
  saving: boolean; saveError?: string;
  /** Source or built-in inspector edits differ from the last successful save. */
  dirty: boolean;
}
export interface LayoutState {
  positions: Record<string, { x: number; y: number }>;
  collapsed: Record<string, boolean>;
  viewport?: { x: number; y: number; zoom: number };
}
export interface ActionField { key: string; label: string; type?: 'text' | 'textarea' | 'boolean' | 'yaml'; required?: boolean }
export interface CustomInspectorContext {
  step: Step; path: Path; readOnly: boolean;
  setField: (path: Path, value: unknown) => void;
}
export interface ActionDefinition {
  uses: string; label: string; fields?: ActionField[];
  renderInspector?: (element: HTMLElement, context: CustomInspectorContext) => void | (() => void);
}
export interface CatalogEntry { id: string; label?: string }
export interface WorkflowOptions {
  source: string; theme?: 'light' | 'dark' | 'system';
  tokens?: Record<`--aw-${string}`, string>;
  agents?: CatalogEntry[]; workflows?: CatalogEntry[]; actions?: ActionDefinition[];
  layout?: LayoutState;
  onLayoutChange?: (layout: LayoutState) => void;
  onChange?: (snapshot: Snapshot) => void;
  onSave?: (snapshot: Snapshot) => void | Promise<void>;
  exportFilename?: string;
  validationKey?: string | number;
  validate?: (snapshot: Snapshot, signal: AbortSignal) => Diagnostic[] | Promise<Diagnostic[]>;
}
export interface WorkflowHandle {
  update: (options: Partial<WorkflowOptions>) => void;
  getSnapshot: () => Snapshot;
  destroy: () => void;
}
