import { expect, it } from 'vitest'

import type { Flag } from './api'
import { unwrapResults, readOnlySql } from './runner'
import { flagsError, networksError, parseFlags, parseNetworks } from './settings'

const flags: Flag[] = [
  { name: 'work_mem', type: 'INTEGER', minValue: '64', maxValue: '1000' },
  { name: 'log_connections', type: 'BOOLEAN' },
  { name: 'pg_stat_statements.track', type: 'STRING', allowedStringValues: ['none', 'top', 'all'] },
  {
    name: 'shared_preload_libraries',
    type: 'REPEATED_STRING',
    allowedStringValues: ['pg_cron', 'pgaudit'],
  },
]

it('parses flags one per line, keeping commas and = in values', () => {
  expect(parseFlags('a=1\n\n shared_preload_libraries = pg_cron,pgaudit \nb=x=y')).toEqual([
    { name: 'a', value: '1' },
    { name: 'shared_preload_libraries', value: 'pg_cron,pgaudit' },
    { name: 'b', value: 'x=y' },
  ])
})

it('explains flags the API would reject, with its messages', () => {
  expect(flagsError('work_mem=100\nlog_connections=on', flags)).toBeUndefined()
  expect(flagsError('shared_preload_libraries=pg_cron, pgaudit', flags)).toBeUndefined()
  expect(flagsError('work_mem', flags)).toMatch(/write name=value/)
  expect(flagsError('work_mem=1', flags)).toBe(
    'Invalid request: Invalid value for flag work_mem: "1".',
  )
  expect(flagsError('log_connections=yes', flags)).toMatch(/Invalid value for flag log_connections/)
  expect(flagsError('pg_stat_statements.track=some', flags)).toMatch(/Invalid value/)
  expect(flagsError('shared_preload_libraries=pg_cron,nope', flags)).toMatch(/Invalid value/)
  expect(flagsError('work_mem=100\nwork_mem=200', flags)).toBe(
    'Invalid request: Duplicate flag: work_mem.',
  )
  expect(flagsError('nope=1', flags)).toBe('Invalid request: Invalid flag name: nope.')
  // Without the list, only the syntax is checked.
  expect(flagsError('nope=1')).toBeUndefined()
})

it('parses and checks authorized networks', () => {
  expect(parseNetworks('office=203.0.113.0/24, 198.51.100.7')).toEqual([
    { name: 'office', value: '203.0.113.0/24' },
    { value: '198.51.100.7' },
  ])
  expect(networksError('0.0.0.0/0\n198.51.100.7')).toBeUndefined()
  expect(networksError('10.0.0.0/33')).toBe(
    'Invalid request: Invalid authorized network (10.0.0.0/33).',
  )
  expect(networksError('300.1.1.1')).toBe(
    'Invalid request: Non-routable or private authorized network (300.1.1.1).',
  )
})

it('wraps read-only statements and drops the results of the wrapper', () => {
  expect(readOnlySql('SELECT 1 -- note')).toBe('BEGIN READ ONLY;\nSELECT 1 -- note\n;\nCOMMIT;')
  expect(
    unwrapResults({
      results: [{ message: 'BEGIN' }, { message: 'SELECT 1' }, { message: 'COMMIT' }],
    }).results,
  ).toEqual([{ message: 'SELECT 1' }])
  // A failed request has no results to drop.
  expect(unwrapResults({ status: { code: 3, message: 'x' } }).results).toEqual([])
})
