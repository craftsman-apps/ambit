// CSV for spreadsheets. Every cell is quoted; a cell that a spreadsheet would run as a formula
// (starting with = + - @, a tab or a carriage return) gets a leading apostrophe, because the audit
// export holds text from requests (tool names, errors) that anyone with a token can influence.
export function csvCell(v: string | number): string {
  let s = String(v);
  if (typeof v === "string" && /^[=+\-@\t\r]/.test(s)) s = "'" + s;
  return `"${s.replace(/"/g, '""')}"`;
}

export function csvRow(cells: (string | number)[]): string {
  return cells.map(csvCell).join(",");
}
