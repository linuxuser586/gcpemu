import type { ExecuteSqlResponse, QueryResult } from './api'

// The query runner's read-only default: the statements run inside
// BEGIN READ ONLY ... COMMIT, so that PostgreSQL refuses writes with
// "cannot execute ... in a read-only transaction". executeSql reports a
// result for each statement, so the runner drops those of the two it
// added. A statement of the user's own (COMMIT, SET TRANSACTION) can still
// leave read-only mode: it guards against accidents, not against intent.

const BEGIN = 'BEGIN READ ONLY;\n'
const END = '\n;\nCOMMIT;'

/** readOnlySql wraps statements in a read-only transaction. */
export const readOnlySql = (sql: string) => BEGIN + sql + END

/** unwrapResults drops the results of the BEGIN and COMMIT readOnlySql added. */
export function unwrapResults(res: ExecuteSqlResponse): ExecuteSqlResponse {
  const results: QueryResult[] = [...(res.results ?? [])]
  if (results[0]?.message === 'BEGIN') results.shift()
  if (results.at(-1)?.message === 'COMMIT') results.pop()
  return { ...res, results }
}

/** DEFAULT_ROW_LIMIT caps the rows the runner shows per statement. */
export const DEFAULT_ROW_LIMIT = 1000
