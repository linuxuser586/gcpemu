# A Cloud SQL Public IP moves only for holders outside the emulator

Clients connect to a Cloud SQL instance's Public IP, so a restart that moves it breaks them. Recovery used to move it whenever a start of its container failed in a way that looked like the address was taken. The runtime words a taken host port the same way, so a host port that another process had taken during the restart moved the address too (#673). We decided that a Public IP moves only when the runtime shows a container that is not the Instance's own holding the address on the External network, or when the address has fallen outside the External network's range. Any other failure, a holder belonging to the Instance included, leaves the Cloud SQL instance FAILED at the same address, and a later start or `instances.restart` tries again. The emulator decides by asking the runtime who holds the address, not by reading the error's wording, which differs between Docker, Podman and Docker Desktop.

## Considered options

- **Move on any start failure.** The Cloud SQL instance stays available, but at an address its clients don't know, and the cause (often a transient error) goes unseen.
- **Never move.** A container someone started by hand on the External network would keep the Cloud SQL instance FAILED until they removed it, and nothing in the emulator could clear it.

## Consequences

- A start error the emulator does not recognise now ends in FAILED, where it used to end in RUNNABLE at a new address.
- Another Service of the Instance holding a Public IP is a reservation bug. The start fails with an error naming the holder instead of working around it, so every path that gives a container an address on the External network must skip reserved addresses, not only container creation.
