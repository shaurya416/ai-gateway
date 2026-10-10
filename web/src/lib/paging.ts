/**
 * Whether a page came back empty only because it starts after the last result.
 *
 * Rows leave a result set under an open page all the time: a key deleted from
 * the last page, log entries ageing out of the time range between two polls, a
 * shared link naming a page that has since emptied. The server answers such a
 * page with no rows and a total that is not zero, and that pair means "nothing
 * here, everything earlier" — not "nothing at all".
 */
export function pastLastPage(offset: number, returned: number, total: number): boolean {
  return returned === 0 && offset > 0 && total > 0
}

/** The offset of the page holding the last of `total` results. */
export function lastPageOffset(total: number, pageSize: number): number {
  if (total <= 0 || pageSize <= 0) return 0
  return Math.floor((total - 1) / pageSize) * pageSize
}
