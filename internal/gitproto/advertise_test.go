package gitproto

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
)

func pkt(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

func TestAdvertisementEmptyUpload(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteAdvertisement(&buf, UploadPack, "refs/heads/main", nil); err != nil {
		t.Fatal(err)
	}
	want := pkt("# service=git-upload-pack\n") + "0000" +
		pkt("0000000000000000000000000000000000000000 capabilities^{}\x00multi_ack_detailed side-band-64k ofs-delta agent=littlekittyvcs\n") +
		"0000"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}
	t.Logf("%q", buf.String())
}

func TestAdvertisementEmptyReceive(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteAdvertisement(&buf, ReceivePack, "refs/heads/main", nil); err != nil {
		t.Fatal(err)
	}
	want := pkt("# service=git-receive-pack\n") + "0000" +
		pkt("0000000000000000000000000000000000000000 capabilities^{}\x00report-status delete-refs atomic ofs-delta side-band-64k no-thin agent=littlekittyvcs\n") +
		"0000"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}
}

func TestAdvertisementRefsUpload(t *testing.T) {
	main, dev, tag := hashOf(0x11), hashOf(0x22), hashOf(0x33)
	var buf bytes.Buffer
	err := WriteAdvertisement(&buf, UploadPack, "refs/heads/main", []Ref{
		{Name: "refs/tags/v1", Hash: tag},
		{Name: "refs/heads/main", Hash: main},
		{Name: "refs/heads/dev", Hash: dev},
	})
	if err != nil {
		t.Fatal(err)
	}
	caps := "multi_ack_detailed side-band-64k ofs-delta agent=littlekittyvcs symref=HEAD:refs/heads/main"
	want := pkt("# service=git-upload-pack\n") + "0000" +
		pkt(hexHash(main)+" HEAD\x00"+caps+"\n") +
		pkt(hexHash(dev)+" refs/heads/dev\n") +
		pkt(hexHash(main)+" refs/heads/main\n") +
		pkt(hexHash(tag)+" refs/tags/v1\n") +
		"0000"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}
}

func TestAdvertisementRefsReceive(t *testing.T) {
	main, dev, tag := hashOf(0x11), hashOf(0x22), hashOf(0x33)
	var buf bytes.Buffer
	err := WriteAdvertisement(&buf, ReceivePack, "refs/heads/main", []Ref{
		{Name: "refs/tags/v1", Hash: tag},
		{Name: "refs/heads/main", Hash: main},
		{Name: "refs/heads/dev", Hash: dev},
	})
	if err != nil {
		t.Fatal(err)
	}
	caps := "report-status delete-refs atomic ofs-delta side-band-64k no-thin agent=littlekittyvcs"
	want := pkt("# service=git-receive-pack\n") + "0000" +
		pkt(hexHash(dev)+" refs/heads/dev\x00"+caps+"\n") +
		pkt(hexHash(main)+" refs/heads/main\n") +
		pkt(hexHash(tag)+" refs/tags/v1\n") +
		"0000"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}
}

func TestAdvertisementHeadTargetMissing(t *testing.T) {
	dev := hashOf(0x22)
	var buf bytes.Buffer
	err := WriteAdvertisement(&buf, UploadPack, "refs/heads/main", []Ref{{Name: "refs/heads/dev", Hash: dev}})
	if err != nil {
		t.Fatal(err)
	}
	want := pkt("# service=git-upload-pack\n") + "0000" +
		pkt(hexHash(dev)+" refs/heads/dev\x00multi_ack_detailed side-band-64k ofs-delta agent=littlekittyvcs\n") +
		"0000"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}
}

func TestAdvertisementRejectsBadRef(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteAdvertisement(&buf, UploadPack, "", []Ref{{Name: "refs/heads/a b", Hash: hashOf(1)}}); err == nil {
		t.Fatal("space in the name was accepted")
	}
	buf.Reset()
	if err := WriteAdvertisement(&buf, UploadPack, "", []Ref{{Name: "HEAD", Hash: hashOf(1)}}); err == nil {
		t.Fatal("HEAD was accepted as a direct ref")
	}
}

func TestAdvertisementPeeledTag(t *testing.T) {
	main, tag, peeled := hashOf(0x11), hashOf(0x33), hashOf(0x44)
	var buf bytes.Buffer
	err := WriteAdvertisement(&buf, UploadPack, "refs/heads/main", []Ref{
		{Name: "refs/heads/main", Hash: main},
		{Name: "refs/tags/v1", Hash: tag, Peeled: &peeled},
	})
	if err != nil {
		t.Fatal(err)
	}
	caps := "multi_ack_detailed side-band-64k ofs-delta agent=littlekittyvcs symref=HEAD:refs/heads/main"
	if !strings.HasPrefix(caps, "multi_ack_detailed ") {
		t.Fatal("multi_ack_detailed is not the first capability")
	}
	want := pkt("# service=git-upload-pack\n") + "0000" +
		pkt(hexHash(main)+" HEAD\x00"+caps+"\n") +
		pkt(hexHash(main)+" refs/heads/main\n") +
		pkt(hexHash(tag)+" refs/tags/v1\n") +
		pkt(hexHash(peeled)+" refs/tags/v1^{}\n") +
		"0000"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}
}

func TestAdvertisementPeeledWithoutHead(t *testing.T) {
	tag, peeled := hashOf(0x33), hashOf(0x44)
	var buf bytes.Buffer
	err := WriteAdvertisement(&buf, UploadPack, "", []Ref{
		{Name: "refs/tags/v1", Hash: tag, Peeled: &peeled},
	})
	if err != nil {
		t.Fatal(err)
	}
	caps := "multi_ack_detailed side-band-64k ofs-delta agent=littlekittyvcs"
	want := pkt("# service=git-upload-pack\n") + "0000" +
		pkt(hexHash(tag)+" refs/tags/v1\x00"+caps+"\n") +
		pkt(hexHash(peeled)+" refs/tags/v1^{}\n") +
		"0000"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}
	adv := packp.NewAdvRefs()
	adv.References["refs/tags/v1"] = plumbing.Hash(tag)
	adv.Peeled["refs/tags/v1"] = plumbing.Hash(peeled)
	var dropped bytes.Buffer
	if err := adv.Encode(&dropped); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dropped.String(), "^{}") {
		t.Fatal("go-git emitted a peeled line for the only ref")
	}
}

func TestAdvertisementReceiveHasNoPeeled(t *testing.T) {
	main, tag, peeled := hashOf(0x11), hashOf(0x33), hashOf(0x44)
	var buf bytes.Buffer
	err := WriteAdvertisement(&buf, ReceivePack, "refs/heads/main", []Ref{
		{Name: "refs/heads/main", Hash: main},
		{Name: "refs/tags/v1", Hash: tag, Peeled: &peeled},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "^{}") {
		t.Fatal("receive-pack advertisement has a peeled line")
	}
	caps := "report-status delete-refs atomic ofs-delta side-band-64k no-thin agent=littlekittyvcs"
	want := pkt("# service=git-receive-pack\n") + "0000" +
		pkt(hexHash(main)+" refs/heads/main\x00"+caps+"\n") +
		pkt(hexHash(tag)+" refs/tags/v1\n") +
		"0000"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}
}

func TestParseService(t *testing.T) {
	if svc, ok := ParseService("git-upload-pack"); !ok || svc != UploadPack {
		t.Fatalf("%s %v", svc, ok)
	}
	if svc, ok := ParseService("git-receive-pack"); !ok || svc != ReceivePack {
		t.Fatalf("%s %v", svc, ok)
	}
	for _, bad := range []string{"", "git-upload-archive", "GIT-UPLOAD-PACK"} {
		if _, ok := ParseService(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func hashOf(b byte) [20]byte {
	var id [20]byte
	for i := range id {
		id[i] = b
	}
	return id
}

func hexHash(id [20]byte) string {
	const digits = "0123456789abcdef"
	var out [40]byte
	for i, b := range id {
		out[i*2] = digits[b>>4]
		out[i*2+1] = digits[b&0x0f]
	}
	return string(out[:])
}
