package install

import "testing"

func TestContainersStorageSourcePreservesQualifiedStore(t *testing.T) {
	qualified := "containers-storage:[overlay@/scratch/containers-root+/scratch/containers-runroot]ghcr.io/tuna-os/yellowfin:gnome"
	if got := containersStorageSource(qualified); got != qualified {
		t.Fatalf("containersStorageSource() = %q, want qualified store unchanged", got)
	}
}

func TestContainersStorageSourceAddsDefaultTransport(t *testing.T) {
	const image = "ghcr.io/tuna-os/yellowfin:gnome"
	if got, want := containersStorageSource(image), "containers-storage:"+image; got != want {
		t.Fatalf("containersStorageSource() = %q, want %q", got, want)
	}
}
