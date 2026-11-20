'use strict'

// setup-gcpemu's stand-in for unzip-stream (#1202). @actions/artifact uses
// unzip-stream only to download artifacts, which the action never does;
// the real package pulls in buffers@0.1.1, which declares no licence.
function stubbed() {
  throw new Error('setup-gcpemu does not download artifacts; unzip-stream is stubbed, see #1202')
}

exports.Parse = stubbed
exports.Extract = stubbed
