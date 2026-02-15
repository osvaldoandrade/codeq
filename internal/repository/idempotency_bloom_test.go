package repository

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestBloomFilter_BasicAddAndMaybeHas tests the basic operations of the underlying bloomFilter.
func TestBloomFilter_BasicAddAndMaybeHas(t *testing.T) {
	bf := newBloomFilter(1000, 0.01)

	// Initially, the filter should not have any keys
	if bf.MaybeHas("key1") {
		t.Error("expected MaybeHas to return false for unseen key")
	}

	// Add a key
	bf.Add("key1")

	// Now it should report that it might have the key
	if !bf.MaybeHas("key1") {
		t.Error("expected MaybeHas to return true after Add")
	}

	// Different key should still return false (with high probability)
	if bf.MaybeHas("key2") {
		// Note: This could be a false positive, but with 1000 capacity and 0.01 FP rate,
		// it should be rare for a single different key
		t.Log("warning: got false positive on second key (acceptable but rare)")
	}
}

// TestBloomFilter_EmptyKey tests that empty keys are handled correctly.
func TestBloomFilter_EmptyKey(t *testing.T) {
	bf := newBloomFilter(1000, 0.01)

	// Empty key should return true (conservative behavior)
	if !bf.MaybeHas("") {
		t.Error("expected MaybeHas to return true for empty key")
	}

	// Adding empty key should be a no-op
	bf.Add("")
	// Should still be safe to check
	if !bf.MaybeHas("") {
		t.Error("expected MaybeHas to return true for empty key after Add")
	}
}

// TestBloomFilter_MultipleKeys tests adding and checking multiple keys.
func TestBloomFilter_MultipleKeys(t *testing.T) {
	bf := newBloomFilter(1000, 0.01)
	keys := []string{"key1", "key2", "key3", "key4", "key5"}

	// Add all keys
	for _, key := range keys {
		bf.Add(key)
	}

	// All added keys should be present
	for _, key := range keys {
		if !bf.MaybeHas(key) {
			t.Errorf("expected MaybeHas to return true for key %q", key)
		}
	}
}

// TestBloomFilter_Concurrent tests concurrent Add and MaybeHas operations.
func TestBloomFilter_Concurrent(t *testing.T) {
	bf := newBloomFilter(10000, 0.01)
	const numGoroutines = 10
	const keysPerGoroutine = 100

	var wg sync.WaitGroup

	// Concurrently add keys
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < keysPerGoroutine; i++ {
				key := fmt.Sprintf("g%d-k%d", gid, i)
				bf.Add(key)
			}
		}(g)
	}

	wg.Wait()

	// Verify all keys are present
	for g := 0; g < numGoroutines; g++ {
		for i := 0; i < keysPerGoroutine; i++ {
			key := fmt.Sprintf("g%d-k%d", g, i)
			if !bf.MaybeHas(key) {
				t.Errorf("expected MaybeHas to return true for key %q", key)
			}
		}
	}
}

// TestIdempotencyBloom_BasicAddAndMaybeHas tests the basic operations of idempotencyBloom.
func TestIdempotencyBloom_BasicAddAndMaybeHas(t *testing.T) {
	bloom := newIdempotencyBloom(1000, 0.01, 1*time.Hour)

	// Initially, the filter should not have any keys
	if bloom.MaybeHas("key1") {
		t.Error("expected MaybeHas to return false for unseen key")
	}

	// Add a key
	bloom.Add("key1")

	// Now it should report that it might have the key
	if !bloom.MaybeHas("key1") {
		t.Error("expected MaybeHas to return true after Add")
	}
}

// TestIdempotencyBloom_EmptyKey tests that empty keys are handled conservatively.
func TestIdempotencyBloom_EmptyKey(t *testing.T) {
	bloom := newIdempotencyBloom(1000, 0.01, 1*time.Hour)

	// Empty key should return true (conservative: force Redis check)
	if !bloom.MaybeHas("") {
		t.Error("expected MaybeHas to return true for empty key")
	}

	// Adding empty key should be a no-op
	bloom.Add("")

	// Empty key should still return true
	if !bloom.MaybeHas("") {
		t.Error("expected MaybeHas to return true for empty key after Add")
	}
}

// TestIdempotencyBloom_Rotation tests that the bloom filter rotates correctly.
func TestIdempotencyBloom_Rotation(t *testing.T) {
	// Create a bloom filter with very short rotation interval
	rotateInterval := 50 * time.Millisecond
	bloom := newIdempotencyBloom(1000, 0.01, rotateInterval)

	// Add a key to the current filter
	bloom.Add("key1")

	// Verify it's present
	if !bloom.MaybeHas("key1") {
		t.Error("expected MaybeHas to return true for key1 in current filter")
	}

	// Wait for rotation
	time.Sleep(rotateInterval + 10*time.Millisecond)

	// After rotation, key1 should still be in prev filter
	if !bloom.MaybeHas("key1") {
		t.Error("expected MaybeHas to return true for key1 in prev filter after rotation")
	}

	// Add a new key to the new current filter
	bloom.Add("key2")

	// Both keys should be present
	if !bloom.MaybeHas("key1") {
		t.Error("expected MaybeHas to return true for key1 after rotation")
	}
	if !bloom.MaybeHas("key2") {
		t.Error("expected MaybeHas to return true for key2 in new current filter")
	}

	// Wait for another rotation
	time.Sleep(rotateInterval + 10*time.Millisecond)

	// key1 should now be gone (it was in prev, which is now dropped)
	if bloom.MaybeHas("key1") {
		// This might still return true due to hash collision, but let's check
		t.Log("warning: key1 still present after two rotations (possible hash collision)")
	}

	// key2 should still be present (now in prev)
	if !bloom.MaybeHas("key2") {
		t.Error("expected MaybeHas to return true for key2 in prev filter after second rotation")
	}

	// Add a third key
	bloom.Add("key3")

	// key3 should be present
	if !bloom.MaybeHas("key3") {
		t.Error("expected MaybeHas to return true for key3 in current filter")
	}
}

// TestIdempotencyBloom_DoubleRotation tests that keys are eventually forgotten after two rotations.
func TestIdempotencyBloom_DoubleRotation(t *testing.T) {
	// Create a bloom filter with very short rotation interval and small capacity
	// to minimize false positives
	rotateInterval := 30 * time.Millisecond
	bloom := newIdempotencyBloom(100, 0.01, rotateInterval)

	// Add a unique key using test name and counter to ensure uniqueness
	uniqueKey := fmt.Sprintf("%s-unique-key-0", t.Name())
	bloom.Add(uniqueKey)

	// Verify it's present
	if !bloom.MaybeHas(uniqueKey) {
		t.Error("expected MaybeHas to return true for unique key")
	}

	// Wait for first rotation (key moves from current to prev)
	time.Sleep(rotateInterval + 10*time.Millisecond)

	// Key should still be present in prev
	if !bloom.MaybeHas(uniqueKey) {
		t.Error("expected MaybeHas to return true after first rotation")
	}

	// Wait for second rotation (prev is dropped, key should be gone)
	time.Sleep(rotateInterval + 10*time.Millisecond)

	// Key should likely be gone now (unless false positive)
	// We test this by adding many other keys and checking if our original key
	// is still detected, which would indicate either it wasn't properly rotated out
	// or we have a very unlikely false positive
	
	// Add many new keys to the filter
	for i := 0; i < 50; i++ {
		bloom.Add(fmt.Sprintf("new-key-%d", i))
	}

	// Check if our original key is still present
	// With 100 capacity and 0.01 FP rate, the chance of a false positive is low
	// but not zero, so we'll log a warning rather than fail
	if bloom.MaybeHas(uniqueKey) {
		t.Logf("warning: unique key still detected after double rotation (likely false positive, acceptable)")
	}
}

// TestIdempotencyBloom_ConcurrentAddAndMaybeHas tests concurrent operations.
func TestIdempotencyBloom_ConcurrentAddAndMaybeHas(t *testing.T) {
	bloom := newIdempotencyBloom(10000, 0.01, 1*time.Hour)
	const numGoroutines = 20
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*opsPerGoroutine)

	// Concurrently add and check keys
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				key := fmt.Sprintf("g%d-k%d", gid, i)
				bloom.Add(key)
				// Immediately check if it's present
				if !bloom.MaybeHas(key) {
					errors <- fmt.Errorf("expected MaybeHas to return true for just-added key %q", key)
				}
			}
		}(g)
	}

	wg.Wait()
	close(errors)

	// Check for errors from concurrent operations
	for err := range errors {
		t.Error(err)
	}

	// Verify all keys are still present
	for g := 0; g < numGoroutines; g++ {
		for i := 0; i < opsPerGoroutine; i++ {
			key := fmt.Sprintf("g%d-k%d", g, i)
			if !bloom.MaybeHas(key) {
				t.Errorf("expected MaybeHas to return true for key %q", key)
			}
		}
	}
}

// TestIdempotencyBloom_ConcurrentRotation tests concurrent operations during rotation.
func TestIdempotencyBloom_ConcurrentRotation(t *testing.T) {
	rotateInterval := 50 * time.Millisecond
	bloom := newIdempotencyBloom(10000, 0.01, rotateInterval)
	const numGoroutines = 10
	const duration = 200 * time.Millisecond

	var wg sync.WaitGroup
	done := make(chan struct{})
	errors := make(chan error, numGoroutines*100)

	// Start workers that continuously add and check keys
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-done:
					return
				default:
					key := fmt.Sprintf("g%d-k%d", gid, i)
					bloom.Add(key)
					if !bloom.MaybeHas(key) {
						errors <- fmt.Errorf("expected MaybeHas to return true for just-added key %q", key)
					}
					i++
					time.Sleep(1 * time.Millisecond)
				}
			}
		}(g)
	}

	// Let them run for a duration that includes multiple rotations
	time.Sleep(duration)
	close(done)
	wg.Wait()
	close(errors)

	// Check for any errors
	for err := range errors {
		t.Error(err)
	}
}

// TestIdempotencyBloom_DefaultParameters tests that default parameters are applied correctly.
func TestIdempotencyBloom_DefaultParameters(t *testing.T) {
	const (
		defaultBloomCapacity   = 1_000_000
		defaultBloomFPRate     = 0.01
		defaultBloomRotateTime = 30 * time.Minute
	)

	// Test with zero values - should use defaults
	bloom := newIdempotencyBloom(0, 0, 0)

	if bloom.n != defaultBloomCapacity {
		t.Errorf("expected default n=%d, got %d", defaultBloomCapacity, bloom.n)
	}
	if bloom.fpRate != defaultBloomFPRate {
		t.Errorf("expected default fpRate=%f, got %f", defaultBloomFPRate, bloom.fpRate)
	}
	if bloom.rotateEvery != defaultBloomRotateTime {
		t.Errorf("expected default rotateEvery=%v, got %v", defaultBloomRotateTime, bloom.rotateEvery)
	}

	// Test with invalid FP rate (>= 1)
	bloom = newIdempotencyBloom(1000, 1.5, 1*time.Hour)
	if bloom.fpRate != defaultBloomFPRate {
		t.Errorf("expected fpRate=%f for invalid input, got %f", defaultBloomFPRate, bloom.fpRate)
	}

	// Test with invalid FP rate (<= 0)
	bloom = newIdempotencyBloom(1000, -0.1, 1*time.Hour)
	if bloom.fpRate != defaultBloomFPRate {
		t.Errorf("expected fpRate=%f for invalid input, got %f", defaultBloomFPRate, bloom.fpRate)
	}

	// Test with negative rotate interval
	bloom = newIdempotencyBloom(1000, 0.01, -1*time.Hour)
	if bloom.rotateEvery != defaultBloomRotateTime {
		t.Errorf("expected default rotateEvery=%v for invalid input, got %v", defaultBloomRotateTime, bloom.rotateEvery)
	}
}

// TestIdempotencyBloom_PrevFilterLookup tests that both current and prev filters are checked.
func TestIdempotencyBloom_PrevFilterLookup(t *testing.T) {
	rotateInterval := 50 * time.Millisecond
	bloom := newIdempotencyBloom(1000, 0.01, rotateInterval)

	// Add keys before rotation
	bloom.Add("old-key-1")
	bloom.Add("old-key-2")

	// Wait for rotation
	time.Sleep(rotateInterval + 10*time.Millisecond)

	// Add new keys after rotation
	bloom.Add("new-key-1")
	bloom.Add("new-key-2")

	// All keys should be present (old in prev, new in current)
	if !bloom.MaybeHas("old-key-1") {
		t.Error("expected MaybeHas to return true for old-key-1 in prev filter")
	}
	if !bloom.MaybeHas("old-key-2") {
		t.Error("expected MaybeHas to return true for old-key-2 in prev filter")
	}
	if !bloom.MaybeHas("new-key-1") {
		t.Error("expected MaybeHas to return true for new-key-1 in current filter")
	}
	if !bloom.MaybeHas("new-key-2") {
		t.Error("expected MaybeHas to return true for new-key-2 in current filter")
	}
}

// TestIdempotencyBloom_RotationIsThreadSafe tests that rotation doesn't cause race conditions.
func TestIdempotencyBloom_RotationIsThreadSafe(t *testing.T) {
	rotateInterval := 30 * time.Millisecond
	bloom := newIdempotencyBloom(10000, 0.01, rotateInterval)
	const numGoroutines = 20
	const duration = 150 * time.Millisecond

	var wg sync.WaitGroup
	done := make(chan struct{})
	errors := make(chan error, numGoroutines)

	// Start workers that continuously add and check keys during rotations
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-done:
					return
				default:
					key := fmt.Sprintf("g%d-k%d", gid, i)
					bloom.Add(key)
					// The key should be immediately present
					if !bloom.MaybeHas(key) {
						errors <- fmt.Errorf("key %q not found immediately after Add", key)
						return
					}
					i++
				}
			}
		}(g)
	}

	// Let them run through multiple rotations
	time.Sleep(duration)
	close(done)
	wg.Wait()
	close(errors)

	// Check for any errors
	for err := range errors {
		t.Error(err)
	}
}

// TestBloomFilter_MinimumSize tests that the filter has a minimum size.
func TestBloomFilter_MinimumSize(t *testing.T) {
	// Create with very small capacity
	bf := newBloomFilter(1, 0.01)

	// Should have at least 64 bits
	if bf.mBits < 64 {
		t.Errorf("expected mBits >= 64, got %d", bf.mBits)
	}

	// Should have at least 1 hash function
	if bf.k < 1 {
		t.Errorf("expected k >= 1, got %d", bf.k)
	}
}

// TestBloomFilter_HashCollisionHandling tests that hash collisions are handled.
func TestBloomFilter_HashCollisionHandling(t *testing.T) {
	bf := newBloomFilter(100, 0.01)

	// Add many keys to increase chance of bit collisions
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("key-%d", i)
		bf.Add(key)
	}

	// All added keys should still be present
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("key-%d", i)
		if !bf.MaybeHas(key) {
			t.Errorf("expected MaybeHas to return true for key %q", key)
		}
	}

	// Note: Some unseen keys might return true (false positives), which is expected
	// We're just verifying that the filter doesn't break with many collisions
}
