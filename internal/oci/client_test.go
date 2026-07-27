package oci_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/embewi/core/internal/oci"
)

// manifestServer sert un manifeste OCI minimal avec une seule layer firmware.
func manifestServer(t *testing.T, layerDigest string, annotations map[string]string) *httptest.Server {
	t.Helper()
	body := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"layers": []map[string]any{
			{"mediaType": oci.MediaTypeFirmwareBin, "digest": layerDigest, "size": 1024},
		},
		"annotations": annotations,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(body) //nolint:errcheck
	}))
}

func imageRef(ts *httptest.Server) string {
	return strings.TrimPrefix(ts.URL, "http://") + "/repo:v1"
}

func TestResolveFirmware_NoTrustedKey_SkipsVerification(t *testing.T) {
	ts := manifestServer(t, "sha256:abc", nil)
	defer ts.Close()

	cli := oci.New()
	meta, err := cli.ResolveFirmware(context.Background(), imageRef(ts))
	if err != nil {
		t.Fatalf("ResolveFirmware sans clé configurée : %v", err)
	}
	if meta.Digest != "sha256:abc" {
		t.Errorf("Digest : got %q, want sha256:abc", meta.Digest)
	}
}

func TestResolveFirmware_TrustedKey_ValidSignature_Accepted(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:abc"
	sig := ed25519.Sign(priv, []byte(digest))
	ts := manifestServer(t, digest, map[string]string{
		"embewi.io/signature": base64.StdEncoding.EncodeToString(sig),
	})
	defer ts.Close()

	cli := oci.New(oci.WithTrustedPublicKey(pub))
	meta, err := cli.ResolveFirmware(context.Background(), imageRef(ts))
	if err != nil {
		t.Fatalf("ResolveFirmware avec signature valide : %v", err)
	}
	if meta.Digest != digest {
		t.Errorf("Digest : got %q, want %q", meta.Digest, digest)
	}
}

func TestResolveFirmware_TrustedKey_MissingAnnotation_Rejected(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := manifestServer(t, "sha256:abc", nil)
	defer ts.Close()

	cli := oci.New(oci.WithTrustedPublicKey(pub))
	_, err = cli.ResolveFirmware(context.Background(), imageRef(ts))
	if !errors.Is(err, oci.ErrSignatureMissing) {
		t.Fatalf("got err %v, want ErrSignatureMissing", err)
	}
}

func TestResolveFirmware_TrustedKey_WrongKey_Rejected(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:abc"
	sig := ed25519.Sign(priv, []byte(digest)) // signé avec une clé différente de otherPub
	ts := manifestServer(t, digest, map[string]string{
		"embewi.io/signature": base64.StdEncoding.EncodeToString(sig),
	})
	defer ts.Close()

	cli := oci.New(oci.WithTrustedPublicKey(otherPub))
	_, err = cli.ResolveFirmware(context.Background(), imageRef(ts))
	if !errors.Is(err, oci.ErrSignatureInvalid) {
		t.Fatalf("got err %v, want ErrSignatureInvalid", err)
	}
}

func TestResolveFirmware_TrustedKey_SignatureOverWrongDigest_Rejected(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Signature valide, mais sur un autre digest que celui annoncé par la layer —
	// simule un manifeste trafiqué après signature (digest substitué).
	sig := ed25519.Sign(priv, []byte("sha256:other"))
	ts := manifestServer(t, "sha256:abc", map[string]string{
		"embewi.io/signature": base64.StdEncoding.EncodeToString(sig),
	})
	defer ts.Close()

	cli := oci.New(oci.WithTrustedPublicKey(pub))
	_, err = cli.ResolveFirmware(context.Background(), imageRef(ts))
	if !errors.Is(err, oci.ErrSignatureInvalid) {
		t.Fatalf("got err %v, want ErrSignatureInvalid", err)
	}
}

// blobServer sert un unique blob à /v2/repo/blobs/<n'importe quel digest> — le digest
// dans l'URL n'est utilisé que pour construire la référence, pas vérifié serveur-side
// (c'est justement StreamBlob côté client qui doit détecter la divergence).
func blobServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content) //nolint:errcheck
	}))
}

func sha256Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestStreamBlob_CorrectDigest_NoMismatch(t *testing.T) {
	content := []byte("firmware binaire de test")
	ts := blobServer(t, content)
	defer ts.Close()

	cli := oci.New()
	image := strings.TrimPrefix(ts.URL, "http://") + "/repo:v1"
	stream, err := cli.StreamBlob(context.Background(), image, sha256Digest(content))
	if err != nil {
		t.Fatalf("StreamBlob : %v", err)
	}
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll : %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("contenu lu : got %q, want %q", got, content)
	}
	if err := stream.Err(); err != nil {
		t.Errorf("Err() après lecture complète et digest correct : %v", err)
	}
	// Close() ne doit jamais porter l'erreur de mismatch (cf. doc BlobStream) — le flux
	// sert typiquement de corps à une requête HTTP sortante dont le transport l'appelle.
	if err := stream.Close(); err != nil {
		t.Errorf("Close() : %v", err)
	}
}

func TestStreamBlob_WrongDigest_ErrReturnsMismatch(t *testing.T) {
	content := []byte("firmware binaire de test")
	ts := blobServer(t, content)
	defer ts.Close()

	cli := oci.New()
	image := strings.TrimPrefix(ts.URL, "http://") + "/repo:v1"
	// Digest attendu délibérément faux (simule un registre compromis servant un blob
	// substitué sous le digest annoncé par le manifeste).
	stream, err := cli.StreamBlob(context.Background(), image, "sha256:0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatalf("StreamBlob : %v", err)
	}
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatalf("ReadAll : %v", err)
	}
	if err := stream.Err(); !errors.Is(err, oci.ErrBlobDigestMismatch) {
		t.Fatalf("Err() : got %v, want ErrBlobDigestMismatch", err)
	}
	// Close() reste muet sur le mismatch même après lecture complète.
	if err := stream.Close(); errors.Is(err, oci.ErrBlobDigestMismatch) {
		t.Errorf("Close() ne doit jamais porter ErrBlobDigestMismatch : %v", err)
	}
}

func TestStreamBlob_PartialRead_ErrDoesNotFlagMismatch(t *testing.T) {
	content := []byte("firmware binaire de test, assez long pour lire partiellement")
	ts := blobServer(t, content)
	defer ts.Close()

	cli := oci.New()
	image := strings.TrimPrefix(ts.URL, "http://") + "/repo:v1"
	// Digest attendu faux, mais on ne lit qu'un octet avant de fermer : aucune
	// conclusion possible sur des données partielles, Err() ne doit pas mentir.
	stream, err := cli.StreamBlob(context.Background(), image, "sha256:0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatalf("StreamBlob : %v", err)
	}
	buf := make([]byte, 1)
	if _, err := stream.Read(buf); err != nil {
		t.Fatalf("Read partiel : %v", err)
	}
	if err := stream.Err(); errors.Is(err, oci.ErrBlobDigestMismatch) {
		t.Errorf("Err() sur lecture partielle ne doit pas conclure à un mismatch : %v", err)
	}
	stream.Close() //nolint:errcheck
}
