/** downloadText saves text as a file of the given type through the browser. */
export function downloadText(filename: string, text: string, type: string) {
  downloadBlob(filename, new Blob([text], { type }))
}

/** downloadBlob saves blob as a file through the browser. */
export function downloadBlob(filename: string, blob: Blob) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.append(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 0)
}
