package isureal

import (
	"bytes"
	"crypto/sha256"
	"testing"
	"testing/synctest"
)

func TestConcurrentImageSignatureClaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &signatureCache{inFlight: make(map[signatureCacheKey]*signatureFlight)}
		r := signatureRequest{[]byte("secret"), "image", 321}
		owners := make(chan bool, 32)
		for range cap(owners) {
			go func() {
				_, flight, owner := cache.claim(r)
				owners <- owner
				if !owner {
					<-flight.done
				}
			}()
		}
		synctest.Wait()
		var count int
		for range cap(owners) {
			if <-owners {
				count++
			}
		}
		cache.finish(r.cacheKey(), []byte("published"))
		if count != 1 {
			t.Fatalf("32 concurrent misses claimed %d computations", count)
		}
		if len(cache.inFlight) != 0 {
			t.Fatal("completed flight retained")
		}
	})
}

func TestScalarAndBatchShareImageSignature(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		key := sha256.Sum256([]byte("owner"))
		otherKey := sha256.Sum256([]byte("second owner"))
		shared := signatureRequest{key[:], newTimeID(), 321}
		independent := []signatureRequest{
			{otherKey[:], shared.imageID, shared.exp},
			{key[:], shared.imageID, shared.exp + 1},
			{key[:], newTimeID(), shared.exp},
		}
		// Hold the shared key as a slow signer. Another batch must publish its
		// other keys, including a duplicate of its own, before joining this one.
		_, _, owner := imageSignatures.claim(shared)
		if !owner {
			t.Fatal("expected cold signature")
		}
		batchDone := make(chan struct{})
		go func() {
			plan := append([]signatureRequest{shared, shared}, independent...)
			computeImageSignatures(append(plan, independent[0]))
			close(batchDone)
		}()
		scalarResult := make(chan []byte, 1)
		go func() { scalarResult <- imageSignature(shared.key, shared.imageID, shared.exp) }()
		synctest.Wait()
		select {
		case <-batchDone:
			t.Fatal("batch returned before the shared signature was published")
		case <-scalarResult:
			t.Fatal("scalar computed the shared signature separately")
		default:
		}
		for _, r := range independent {
			v, _, _ := imageSignatures.claim(r)
			if !bytes.Equal(v, computeImageSignatureGeneric(r.key, r.imageID, r.exp)) {
				t.Fatalf("independent signature missing or incorrect for %v", r.cacheKey())
			}
		}
		expected := computeImageSignatureGeneric(shared.key, shared.imageID, shared.exp)
		imageSignatures.finish(shared.cacheKey(), expected)
		<-batchDone
		if got := <-scalarResult; !bytes.Equal(got, expected) {
			t.Fatal("scalar received a different signature")
		}
	})
}
