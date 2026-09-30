import { expect, test } from "@playwright/test";
import { csvCell, csvRow } from "../src/lib/csv";

// Review finding (Codex): the audit export must not carry spreadsheet formulas from logged text.
test("CSV cells cannot become spreadsheet formulas", () => {
  expect(csvCell("=HYPERLINK(\"http://evil\")")).toBe(`"'=HYPERLINK(""http://evil"")"`);
  for (const p of ["+1", "-1+2", "@SUM(A1)", "\tcmd", "\rx"]) expect(csvCell(p).startsWith(`"'`)).toBe(true);
  expect(csvCell("tools/call · echo")).toBe(`"tools/call · echo"`);
  expect(csvCell(42)).toBe(`"42"`);
  expect(csvCell(-3)).toBe(`"-3"`); // numbers stay numbers
  expect(csvRow(["a", "b,c"])).toBe(`"a","b,c"`);
});
