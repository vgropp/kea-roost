// Types for the type check of internal/web/static. Not served: the browser only
// gets the files in static.

interface Window {
  /** Set by theme.js, which runs before app.js. */
  applyTheme(): void;
}

/** A column of dataTable. */
interface Column {
  key: string;
  label: string;
  class?: string;
  sort?: (row: any) => unknown;
  /** The header does not offer sorting. */
  noSort?: boolean;
  search?: (row: any) => unknown;
  render?: (row: any) => unknown;
}

/** A filter above a dataTable. */
interface Filter {
  key: string;
  label: string;
  default?: string;
  options: (rows: any[]) => { value: string; label: string }[];
  match: (row: any, value: string) => boolean;
}

interface DataTableOptions {
  columns: Column[];
  search?: string;
  filters?: Filter[];
  empty?: string;
  initialSort?: string;
  actions?: ((row: any) => unknown)[];
  params?: URLSearchParams;
  rowClass?: (row: any) => string;
  plain?: boolean;
  pageSize?: number;
}
