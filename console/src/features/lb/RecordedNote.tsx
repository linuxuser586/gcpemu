/**
 * RecordedNote says that a kind of target proxy is Recorded: the emulator
 * stores it, but its load balancer proxies HTTP and HTTPS only.
 */
export function RecordedNote() {
  return (
    <p role="note" className="text-sm text-muted-foreground">
      Recorded: stored and returned. A forwarding rule that targets this proxy carries no Data plane
      traffic; the emulator’s load balancer proxies HTTP and HTTPS only.
    </p>
  )
}
