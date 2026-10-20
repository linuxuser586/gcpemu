/**
 * RecordedNotice says that firewall rules and routes are Recorded: the
 * emulator's VPC container networks do not enforce them.
 */
export function RecordedNotice() {
  return (
    <p role="note" className="text-sm text-muted-foreground">
      Recorded: stored and returned, not enforced by the emulator. Use a Kubernetes NetworkPolicy to
      restrict GKE pod traffic.
    </p>
  )
}
