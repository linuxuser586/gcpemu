import { describe, expect, it } from 'vitest'

import { stepLog } from './api'

// A Build log as the build Service writes it: Cloud Build's own lines
// around each step's output, every output line after its step's label.
const LOG = [
  'starting build "b1"',
  '',
  'FETCHSOURCE',
  'BUILD',
  'Starting Step #0 - "compile"',
  'Step #0 - "compile": go build ./...',
  'Step #0 - "compile": ok',
  'Finished Step #0 - "compile"',
  'Starting Step #1',
  'Step #1: hello',
  'Step #1:   indented: kept',
  'Finished Step #1',
  'Starting Step #10',
  'Step #10: tenth',
  'Finished Step #10',
  'DONE',
  '',
].join('\n')

describe('stepLog', () => {
  it('keeps a step’s output lines, without their label', () => {
    expect(stepLog(LOG, 0, 'compile')).toBe('go build ./...\nok')
  })

  it('reads a step without an ID by its number alone', () => {
    expect(stepLog(LOG, 1)).toBe('hello\n  indented: kept')
  })

  it('does not take step 10’s lines for step 1', () => {
    expect(stepLog(LOG, 10)).toBe('tenth')
  })

  it('is empty for a step that has written nothing yet', () => {
    expect(stepLog(LOG, 2, 'push')).toBe('')
    expect(stepLog('', 0)).toBe('')
  })
})
