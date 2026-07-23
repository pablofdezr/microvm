package microvm_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	microvm "github.com/pablofdezr/microvm-sdk-go/microvm"
)

// The golden path: hold a sandbox, run a command in it, read the output, tear it
// down. A sandbox is a reservation, so remember to delete it.
func Example() {
	client := microvm.New(microvm.DefaultBaseURL, microvm.WithToken("sk_live_..."))
	ctx := context.Background()

	sb, err := client.Sandboxes.Create(ctx, microvm.SandboxCreateParams{Image: "python"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Sandboxes.Delete(ctx, sb.Id)

	exe, err := client.Run(ctx, sb.Id, "python3", "-c", "print('hello from a microVM')")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(exe.Stdout)
}

// A task runs on any node with room for it, no sandbox to hold. Optional fields
// are pointers, so microvm.Ptr sets them without a throwaway variable each.
func Example_task() {
	client := microvm.New(microvm.DefaultBaseURL, microvm.WithToken("sk_live_..."))
	ctx := context.Background()

	task, err := client.Tasks.Create(ctx, microvm.TaskCreateParams{
		Image:    "python",
		Cmd:      "python3",
		Args:     &[]string{"-c", "print(2 + 2)"},
		Vcpus:    microvm.Ptr(2),
		MemMib:   microvm.Ptr(1024),
		Priority: microvm.Ptr(7), // 0-10; higher runs first
	})
	if err != nil {
		log.Fatal(err)
	}

	// Wait for it to finish, wherever in the fleet it ran.
	done, err := client.Tasks.Wait(ctx, task.Id)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(done.Stdout)
}

// Admin-only: set a tenant's storage cap and what a write does when it is full.
func ExampleTenantService_SetLimit() {
	admin := microvm.New(microvm.DefaultBaseURL, microvm.WithToken("sk_admin_..."))
	ctx := context.Background()

	_, err := admin.Tenants.SetLimit(ctx, "t_9f86d0818...", 500<<20, microvm.Evict)
	if microvm.IsForbidden(err) {
		log.Fatal("this key may not set tenant policies")
	}
}

// Resilience: retry transient failures, and watch every attempt. The retry only
// applies to idempotent calls; a keyless create is never repeated.
func Example_resilience() {
	client := microvm.New(microvm.DefaultBaseURL,
		microvm.WithToken("sk_live_..."),
		microvm.WithMaxRetries(4),
		microvm.WithObserver(func(info microvm.RequestInfo) {
			log.Printf("%s %s attempt=%d status=%d in %s",
				info.Method, info.Path, info.Attempt, info.Status, info.Duration)
		}),
	)

	// A capacity error is the one worth retrying as a task instead of a sandbox.
	_, err := client.Sandboxes.Create(context.Background(), microvm.SandboxCreateParams{Image: "python"})
	if microvm.IsCapacity(err) {
		log.Println("node is full; submit a task instead")
	}
}

// Pagination without threading cursors: All follows has_more to the end.
func Example_pagination() {
	client := microvm.New(microvm.DefaultBaseURL, microvm.WithToken("sk_live_..."))

	for sb, err := range client.Sandboxes.All(context.Background(), microvm.SandboxListParams{}) {
		if err != nil {
			if errors.Is(err, context.Canceled) {
				break
			}
			log.Fatal(err)
		}
		fmt.Println(sb.Id, sb.State)
	}
}
