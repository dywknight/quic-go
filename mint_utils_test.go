package quic

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"

	"github.com/lucas-clemente/quic-go/internal/crypto"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/testdata"
	"github.com/lucas-clemente/quic-go/internal/utils"
	"github.com/lucas-clemente/quic-go/internal/wire"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Packing and unpacking Initial packets", func() {
	var (
		aead   crypto.AEAD
		hdrRaw []byte
	)

	connID := protocol.ConnectionID{1, 2, 3, 4, 5, 6, 7, 8}
	ver := protocol.VersionTLS
	hdr := &wire.Header{
		IsLongHeader:     true,
		Type:             protocol.PacketTypeRetry,
		PacketNumber:     0x42,
		PacketNumberLen:  protocol.PacketNumberLen1,
		DestConnectionID: connID,
		SrcConnectionID:  connID,
		Version:          ver,
	}

	BeforeEach(func() {
		var err error
		aead, err = crypto.NewNullAEAD(protocol.PerspectiveServer, connID, ver)
		Expect(err).ToNot(HaveOccurred())
		buf := &bytes.Buffer{}
		Expect(hdr.Write(buf, protocol.PerspectiveClient, ver)).To(Succeed())
		hdr.ParsedLen = buf.Len()
		hdrRaw = buf.Bytes()
	})

	Context("generating a mint.Config", func() {
		It("sets non-blocking mode", func() {
			mintConf, err := tlsToMintConfig(nil, protocol.PerspectiveClient)
			Expect(err).ToNot(HaveOccurred())
			Expect(mintConf.NonBlocking).To(BeTrue())
		})

		It("sets the certificate chain", func() {
			tlsConf := testdata.GetTLSConfig()
			mintConf, err := tlsToMintConfig(tlsConf, protocol.PerspectiveClient)
			Expect(err).ToNot(HaveOccurred())
			Expect(mintConf.Certificates).ToNot(BeEmpty())
			Expect(mintConf.Certificates).To(HaveLen(len(tlsConf.Certificates)))
		})

		It("copies values from the tls.Config", func() {
			verifyErr := errors.New("test err")
			certPool := &x509.CertPool{}
			tlsConf := &tls.Config{
				RootCAs:            certPool,
				ServerName:         "www.example.com",
				InsecureSkipVerify: true,
				VerifyPeerCertificate: func(_ [][]byte, _ [][]*x509.Certificate) error {
					return verifyErr
				},
			}
			mintConf, err := tlsToMintConfig(tlsConf, protocol.PerspectiveClient)
			Expect(err).ToNot(HaveOccurred())
			Expect(mintConf.RootCAs).To(Equal(certPool))
			Expect(mintConf.ServerName).To(Equal("www.example.com"))
			Expect(mintConf.InsecureSkipVerify).To(BeTrue())
			Expect(mintConf.VerifyPeerCertificate(nil, nil)).To(MatchError(verifyErr))
		})

		It("requires client authentication", func() {
			mintConf, err := tlsToMintConfig(nil, protocol.PerspectiveClient)
			Expect(err).ToNot(HaveOccurred())
			Expect(mintConf.RequireClientAuth).To(BeFalse())
			conf := &tls.Config{ClientAuth: tls.RequireAnyClientCert}
			mintConf, err = tlsToMintConfig(conf, protocol.PerspectiveClient)
			Expect(err).ToNot(HaveOccurred())
			Expect(mintConf.RequireClientAuth).To(BeTrue())
		})

		It("rejects unsupported client auth types", func() {
			conf := &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert}
			_, err := tlsToMintConfig(conf, protocol.PerspectiveClient)
			Expect(err).To(MatchError("mint currently only support ClientAuthType RequireAnyClientCert"))
		})
	})

	Context("unpacking", func() {
		packPacket := func(frames []wire.Frame) []byte {
			buf := bytes.NewBuffer(hdrRaw)
			payloadStartIndex := buf.Len()
			aeadCl, err := crypto.NewNullAEAD(protocol.PerspectiveClient, connID, ver)
			Expect(err).ToNot(HaveOccurred())
			for _, f := range frames {
				err := f.Write(buf, ver)
				Expect(err).ToNot(HaveOccurred())
			}
			raw := buf.Bytes()
			data := aeadCl.Seal(raw[payloadStartIndex:payloadStartIndex], raw[payloadStartIndex:], hdr.PacketNumber, raw[:payloadStartIndex])
			return append(raw[:hdr.ParsedLen], data...)
		}

		It("unpacks a packet", func() {
			f := &wire.StreamFrame{
				StreamID: 0,
				Data:     []byte("foobar"),
			}
			p := packPacket([]wire.Frame{f})
			frame, err := unpackInitialPacket(aead, hdr, p, utils.DefaultLogger, ver)
			Expect(err).ToNot(HaveOccurred())
			Expect(frame).To(Equal(f))
		})

		It("rejects a packet that doesn't contain a STREAM_FRAME", func() {
			p := packPacket([]wire.Frame{&wire.PingFrame{}})
			_, err := unpackInitialPacket(aead, hdr, p, utils.DefaultLogger, ver)
			Expect(err).To(MatchError("Packet doesn't contain a STREAM_FRAME"))
		})

		It("rejects a packet that has a STREAM_FRAME for the wrong stream", func() {
			f := &wire.StreamFrame{
				StreamID: 42,
				Data:     []byte("foobar"),
			}
			p := packPacket([]wire.Frame{f})
			_, err := unpackInitialPacket(aead, hdr, p, utils.DefaultLogger, ver)
			Expect(err).To(MatchError("Received STREAM_FRAME for wrong stream (Stream ID 42)"))
		})

		It("rejects a packet that has a STREAM_FRAME with a non-zero offset", func() {
			f := &wire.StreamFrame{
				StreamID: 0,
				Offset:   10,
				Data:     []byte("foobar"),
			}
			p := packPacket([]wire.Frame{f})
			_, err := unpackInitialPacket(aead, hdr, p, utils.DefaultLogger, ver)
			Expect(err).To(MatchError("received stream data with non-zero offset"))
		})
	})

	Context("packing", func() {
		It("packs a packet", func() {
			f := &wire.StreamFrame{
				Data:   []byte("foobar"),
				FinBit: true,
			}
			data, err := packUnencryptedPacket(aead, hdr, f, protocol.PerspectiveServer, utils.DefaultLogger)
			Expect(err).ToNot(HaveOccurred())
			aeadCl, err := crypto.NewNullAEAD(protocol.PerspectiveClient, connID, ver)
			Expect(err).ToNot(HaveOccurred())
			decrypted, err := aeadCl.Open(nil, data[hdr.ParsedLen:], hdr.PacketNumber, data[:hdr.ParsedLen])
			Expect(err).ToNot(HaveOccurred())
			frame, err := wire.ParseNextFrame(bytes.NewReader(decrypted), hdr.PacketNumber, hdr.PacketNumberLen, versionIETFFrames)
			Expect(err).ToNot(HaveOccurred())
			Expect(frame).To(Equal(f))
		})
	})
})
