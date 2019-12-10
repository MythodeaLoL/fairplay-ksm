package ksm

import (
	"crypto/aes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Cooomma/ksm/crypto"
)

// SPCContainer represents a container to contain SPC message filed.
type SPCContainer struct {
	Version           uint32
	Reserved          []byte
	AesKeyIV          []byte
	EncryptedAesKey   []byte
	CertificateHash   []byte
	SPCPlayload       []byte
	SPCPlayloadLength uint32

	TTLVS map[uint64]TLLVBlock
}

// CKCContainer represents a container to contain CKC message filed.
type CKCContainer struct {
	CKCVersion       uint32 //0x00000001
	Reserved         []byte
	CKCDataInitV     []byte //A random 16-byte initialization vector, generated by the KSM
	CKCPayload       []byte //A variable-length set of contiguous TLLV blocks
	CKCPayloadLength uint32 //The number of bytes in the encrypted CKC payload.
}

// Serialize serializes CKCContainer message byte array.
func (c *CKCContainer) Serialize() []byte {
	var out []byte

	versionOut := make([]byte, 4)
	binary.BigEndian.PutUint32(versionOut, c.CKCVersion)

	payloadLenOut := make([]byte, 4)
	payloadLen := uint32(len(c.CKCPayload))
	fmt.Printf("payloadLen:%v\n", payloadLen)
	binary.BigEndian.PutUint32(payloadLenOut, payloadLen)

	out = append(out, versionOut...)
	out = append(out, c.Reserved...)
	out = append(out, c.CKCDataInitV...)
	out = append(out, payloadLenOut...)
	out = append(out, c.CKCPayload...)
	return out

}

// Ksm represents a ksm object.
type Ksm struct {
	Pub *rsa.PublicKey
	Pri *rsa.PrivateKey
	Rck ContentKey
	Ask []byte
	d   DFunction
}

// GenCKC computes the incoming server playback context (SPC message) returned to client by the SKDServer library.
func (k *Ksm) GenCKC(playback []byte) ([]byte, error) {
	spcv1, err := ParseSPCV1(playback, k.Pub, k.Pri)
	if err != nil {
		return nil, err
	}

	ttlvs := spcv1.TTLVS
	skr1 := parseSKR1(ttlvs[tagSessionKeyR1])

	r2 := ttlvs[tagR2]
	dask, err := k.d.Compute(r2.Value, k.Ask)

	if err != nil {
		return nil, err
	}
	fmt.Printf("DASk Value:\n\t%s\n\n", hex.EncodeToString(dask))

	DecryptedSKR1Payload, err := decryptSKR1Payload(*skr1, dask)
	if err != nil {
		return nil, err
	}

	fmt.Printf("Decrypted SKR1Payload: %s\n", hex.EncodeToString(DecryptedSKR1Payload.IntegrityBytes))

	//FIXME: Have to complete this cksum.
	//Check the integrity of this SPC message
	/*
		checkTheIntegrity, ok := ttlvs[tagSessionKeyR1Integrity]
		if !ok {
			return nil, errors.New("tagSessionKeyR1Integrity block doesn't existed")
		}

		fmt.Printf("checkTheIntegrity: %s\n", hex.EncodeToString(checkTheIntegrity.Value))

		if !reflect.DeepEqual(checkTheIntegrity.Value, DecryptedSKR1Payload.IntegrityBytes) {
			return nil, errors.New("check the integrity of the SPC failed")
		}
	*/
	fmt.Printf("DASk Value:\n\t%s\n\n", hex.EncodeToString(dask))
	fmt.Printf("SPC SK Value:\n\t%s\n\n", hex.EncodeToString(DecryptedSKR1Payload.SK))
	fmt.Printf("SPC [SK..R1] IV Value:\n\t%s\n\n", hex.EncodeToString(skr1.IV))
	//fmt.Printf("SPC R1 Value:\n%s\n\n",hex.EncodeToString(DecryptedSKR1Payload.R1))

	assetTTlv := ttlvs[tagAssetID]

	// assetID its length can range from 2 to 200 bytes
	if assetTTlv.ValueLength < 2 || assetTTlv.ValueLength > 200 {
		return nil, errors.New("assetID its length must be range from 2 to 200 bytes")
	}

	assetID := assetTTlv.Value
	fmt.Printf("assetID: %v\n", hex.EncodeToString(assetID))
	fmt.Printf("assetID(string): %v\n", string(assetID))

	enCk, contentIv, err := encryptCK(assetTTlv.Value, k.Rck, DecryptedSKR1Payload.SK)
	if err != nil {
		return nil, err
	}
	fmt.Println("enCK Length ", len(enCk))

	returnTllvs := findReturnRequestBlocks(spcv1)

	ckcDataIv := generateRandomIv()
	ckcR1 := CkcR1{
		R1: DecryptedSKR1Payload.R1,
	}

	fmt.Println("ckcDataIV Length", len(ckcDataIv.IV))

	encryptedArSeed, err := getEncryptedArSeed(DecryptedSKR1Payload.R1, ttlvs[tagAntiReplaySeed].Value)
	if err != nil {
		return nil, err
	}

	ckcPayload, err := genCkcPayload(contentIv, enCk, ckcR1, returnTllvs)
	if err != nil {
		return nil, err
	}

	//ContenKeyDurationTllv,  This TLLV may be present only if the KSM has received an SPC with a Media Playback State TLLV.
	if _, ok := ttlvs[tagMediaPlaybackState]; ok {
		ckcDuraionTllv, err := genCkDurationTllv(assetID, k.Rck)
		if err != nil {
			return nil, err
		}

		ckcPayload = append(ckcPayload, ckcDuraionTllv...)
	}

	enCkcPayload, err := encryptCkcPayload(encryptedArSeed, ckcDataIv, ckcPayload)
	if err != nil {
		return nil, err
	}

	out := fillCKCContainer(enCkcPayload.Payload, ckcDataIv)
	return out, nil
}

func genCkDurationTllv(assetID []byte, key ContentKey) ([]byte, error) {
	CkcContentKeyDurationBlock, err := key.FetchContentKeyDuration(assetID)
	if err != nil {
		return nil, err
	}
	return CkcContentKeyDurationBlock.Serialize()
}

// DebugCKC debbug ckcplayback content
func DebugCKC(ckcplayback []byte) {
	ckcContaniner := &CKCContainer{}
	ckcContaniner.CKCVersion = binary.BigEndian.Uint32(ckcplayback[0:4])
	ckcContaniner.Reserved = ckcplayback[4:8]
	ckcContaniner.CKCDataInitV = ckcplayback[8:24]
	ckcContaniner.CKCPayloadLength = binary.BigEndian.Uint32(ckcplayback[24:28])
	ckcContaniner.CKCPayload = ckcplayback[28 : 28+ckcContaniner.CKCPayloadLength]
}

func genCkcPayload(ckIv, enCk []byte, ckcR1 CkcR1, returnTllvs []TLLVBlock) ([]byte, error) {
	//TODO: The order of these blocks should be random.
	var ckcPayload []byte

	//Content Key TLLV
	var contentKeyTllv []byte

	tagOut := make([]byte, 8)
	binary.BigEndian.PutUint64(tagOut, tagEncryptedCk)
	contentKeyTllv = append(contentKeyTllv, tagOut...)
	contentKeyTllv = append(contentKeyTllv, []byte{0x00, 0x00, 0x00, 0x30}...) // Block length: Value(32) + Padding(16)
	contentKeyTllv = append(contentKeyTllv, []byte{0x00, 0x00, 0x00, 0x20}...) // Value length: IV(16) + CK(16)
	contentKeyTllv = append(contentKeyTllv, ckIv...)                           // 16-byte CBC initialization vector used in AES encryption and decryption of audio and video assets.
	contentKeyTllv = append(contentKeyTllv, enCk...)

	padding := make([]byte, 16)
	rand.Read(padding)
	contentKeyTllv = append(contentKeyTllv, padding...)

	if len(contentKeyTllv) != 64 {
		return nil, errors.New("contentKeyTllv len must be 64")
	}

	ckcPayload = append(ckcPayload, contentKeyTllv...)

	//R1Tllv
	r1TllvBlock := NewTLLVBlock(tagR1, ckcR1.R1)

	r1TllvBlockOut, err := r1TllvBlock.Serialize()
	if err != nil {
		return nil, err
	}
	ckcPayload = append(ckcPayload, r1TllvBlockOut...)

	// serializeReturnRequesTllvs
	for _, rtnTlv := range returnTllvs {
		rtnTlvOut, err := rtnTlv.Serialize()
		if err != nil {
			return nil, err
		}
		ckcPayload = append(ckcPayload, rtnTlvOut...)
	}

	return ckcPayload, nil

}

func getEncryptedArSeed(r1 []byte, arSeed []byte) ([]byte, error) {
	h := sha1.New()
	h.Write(r1)
	arKey := h.Sum(nil)[0:16]

	return crypto.AESECBEncrypt(arKey, arSeed)
}

func generateRandomIv() CkcDataIv {
	key := make([]byte, aes.BlockSize)
	rand.Read(key)

	return CkcDataIv{
		IV: key,
	}

}

func encryptCkcPayload(encryptedArSeed []byte, iv CkcDataIv, ckcPayload []byte) (*CkcEncryptedPayload, error) {

	encryped, err := crypto.AESCBCEncrypt(encryptedArSeed, iv.IV, ckcPayload)
	if err != nil {
		return nil, err
	}
	return &CkcEncryptedPayload{Payload: encryped}, nil
}

func findReturnRequestBlocks(spcv1 *SPCContainer) []TLLVBlock {
	tagReturnReq := spcv1.TTLVS[tagReturnRequest]

	var returnTllvs []TLLVBlock

	for currentOffset := 0; currentOffset < len(tagReturnReq.Value); {
		tag := binary.BigEndian.Uint64(tagReturnReq.Value[currentOffset : currentOffset+8])

		if ttlv, ok := spcv1.TTLVS[tag]; ok {
			fmt.Printf("tag: %x\n", tagReturnReq.Value[currentOffset:currentOffset+8])
			returnTllvs = append(returnTllvs, ttlv)
		} else {
			fmt.Printf("no tag: %x\n", tagReturnReq.Value[currentOffset:currentOffset+8])
			panic("Can not found  tag")
		}

		currentOffset += fieldTagLength
	}

	return returnTllvs
}

func encryptCK(assetID []byte, ck ContentKey, sk []byte) ([]byte, []byte, error) {
	contentKey, contentIv, err := ck.FetchContentKey(assetID)
	if err != nil {
		return nil, nil, err
	}

	var iv []byte
	iv = make([]byte, len(contentKey))

	//enCk, err := aes.Encrypt(sk, iv, contentKey)
	fmt.Println("encryptCK.", len(sk), len(iv), len(contentKey))
	enCK, err := crypto.AESCBCEncrypt(sk, iv, contentKey)
	fmt.Println("encryptCK.", len(enCK))
	return enCK, contentIv, err
}

// ParseSPCV1 parses playback, public and private key pairs to new a SPCContainer instance.
// ParseSPCV1 returns an error if playback can't be parsed.
func ParseSPCV1(playback []byte, pub *rsa.PublicKey, pri *rsa.PrivateKey) (*SPCContainer, error) {
	spcContainer := parseSPCContainer(playback)

	spck, err := decryptSPCK(pub, pri, spcContainer.EncryptedAesKey)
	if err != nil {
		return nil, err
	}

	printDebugSPC(spcContainer)

	spcPayload, err := crypto.AESCBCDecrypt(spck, spcContainer.AesKeyIV, spcContainer.SPCPlayload)
	if err != nil {
		return nil, err
	}
	fmt.Println("=== SPC PayloadRow ===")
	fmt.Println(hex.EncodeToString(spcPayload))

	spcContainer.TTLVS = parseTLLVs(spcPayload)

	return spcContainer, nil
}

func parseSPCContainer(playback []byte) *SPCContainer {
	spcContainer := &SPCContainer{}
	spcContainer.Version = binary.BigEndian.Uint32(playback[0:4])
	spcContainer.Reserved = playback[4:8]
	spcContainer.AesKeyIV = playback[8:24]
	spcContainer.EncryptedAesKey = playback[24:152]
	spcContainer.CertificateHash = playback[152:172]
	spcContainer.SPCPlayloadLength = binary.BigEndian.Uint32(playback[172:176])
	spcContainer.SPCPlayload = playback[176 : 176+spcContainer.SPCPlayloadLength]

	return spcContainer
}

func fillCKCContainer(CkcEncryptedPayload []byte, iv CkcDataIv) []byte {
	ckcContaniner := CKCContainer{
		CKCVersion:   0x00000001,
		Reserved:     []byte{0x00, 0x00, 0x00, 0x00},
		CKCDataInitV: iv.IV,
		CKCPayload:   CkcEncryptedPayload,
	}

	return ckcContaniner.Serialize()
}

func parseTLLVs(spcPayload []byte) map[uint64]TLLVBlock {
	var m map[uint64]TLLVBlock
	m = make(map[uint64]TLLVBlock)

	for currentOffset := 0; currentOffset < len(spcPayload); {

		tag := binary.BigEndian.Uint64(spcPayload[currentOffset : currentOffset+fieldTagLength])
		currentOffset += fieldTagLength

		blockLength := binary.BigEndian.Uint32(spcPayload[currentOffset : currentOffset+fieldBlockLength])
		currentOffset += fieldBlockLength

		valueLength := binary.BigEndian.Uint32(spcPayload[currentOffset : currentOffset+fieldValueLength])
		currentOffset += fieldValueLength
		//paddingSize := blockLength - valueLength

		value := spcPayload[currentOffset : currentOffset+int(valueLength)]

		var skip bool
		switch tag {
		case tagSessionKeyR1:
			fmt.Printf("tagSessionKeyR1 -- %x\n", tag)
		case tagSessionKeyR1Integrity:
			fmt.Printf("tagSessionKeyR1Integrity -- %x\n", tag)
		case tagAntiReplaySeed:
			fmt.Printf("tagAntiReplaySeed -- %x\n", tag)
		case tagR2:
			fmt.Printf("tagR2 -- %x\n", tag)
		case tagReturnRequest:
			fmt.Printf("tagReturnRequest -- %x\n", tag)
		case tagAssetID:
			fmt.Printf("tagAssetID -- %x\n", tag)
		case tagTransactionID:
			fmt.Printf("tagTransactionID -- %x\n", tag)
		case tagProtocolVersionsSupported:
			fmt.Printf("tagProtocolVersionsSupported -- %x\n", tag)
		case tagProtocolVersionUsed:
			fmt.Printf("tagProtocolVersionUsed -- %x\n", tag)
		case tagTreamingIndicator:
			fmt.Printf("tagTreamingIndicator -- %x\n", tag)
		case tagMediaPlaybackState:
			fmt.Printf("tagMediaPlaybackState -- %x\n", tag)
		default:
			skip = true
		}

		if skip == false {
			fmt.Printf("Tag size:0x%x\n", valueLength)
			fmt.Printf("Tag length:0x%x\n", blockLength)
			fmt.Printf("Tag value:%s\n\n", hex.EncodeToString(value))

			if tag == tagMediaPlaybackState {
				creationDate := binary.BigEndian.Uint32(value[0:4])
				playbackState := binary.BigEndian.Uint32(value[4:8])
				sessionID := binary.BigEndian.Uint32(value[8:12])
				fmt.Printf("\t\t\tSPC creation time - %v\n", creationDate)

				switch playbackState {
				case playbackStateReadyToStart:
					fmt.Println("\t\tPlayback_State_ReadyToStart.")
				case playbackStatePlayingOrPaused:
					fmt.Println("\t\tPlayback_State_PlayingOrPaused.")
				case playbackStatePlaying:
					fmt.Println("\t\tPlayback_State_Playing.")
				case playbackStateHalted:
					fmt.Println("\t\tPlayback_State_Halted.")
				default:
					fmt.Println("not expected.")
				}
				fmt.Printf("%x\n", playbackState)
				fmt.Printf("\t\t\tPlayback Session Id - %v\n", sessionID)
			}
		}

		tllvBlock := TLLVBlock{
			Tag:         tag,
			BlockLength: blockLength,
			ValueLength: valueLength,
			Value:       value,
		}

		m[tag] = tllvBlock

		currentOffset = currentOffset + int(blockLength)
	}

	return m
}

func parseSKR1(tllv TLLVBlock) *SKR1TLLVBlock {
	return &SKR1TLLVBlock{
		TLLVBlock: tllv,
		IV:        tllv.Value[0:16],
		Payload:   tllv.Value[16:112],
	}
}

func decryptSKR1Payload(skr1 SKR1TLLVBlock, dask []byte) (*DecryptedSKR1Payload, error) {
	if skr1.Tag != tagSessionKeyR1 {
		return nil, errors.New("decryptSKR1 doesn't match tagSessionKeyR1 tag")
	}

	// decryptPayloadRow, err := aes.Decrypt(dask, skr1.IV, skr1.Payload)
	decryptPayloadRow, err := crypto.AESCBCDecrypt(dask, skr1.IV, skr1.Payload)
	if err != nil {
		return nil, err
	}

	fmt.Println("Decrypt Payload Row Length: ", len(decryptPayloadRow))

	if len(decryptPayloadRow) != 96 {
		return nil, errors.New("wrong decrypt payload size. Must be 96 bytes expected")
	}

	d := &DecryptedSKR1Payload{
		SK:             decryptPayloadRow[0:16],
		HU:             decryptPayloadRow[16:36],
		R1:             decryptPayloadRow[36:80],
		IntegrityBytes: decryptPayloadRow[80:96],
	}

	return d, nil
}

func printDebugSPC(spcContainer *SPCContainer) {
	fmt.Println("========================= Begin SPC Data ===============================")
	fmt.Printf("SPC container size %+v\n", spcContainer.SPCPlayloadLength)

	fmt.Println("SPC Encryption Key -")
	fmt.Println(hex.EncodeToString(spcContainer.EncryptedAesKey))
	fmt.Println("SPC Encryption IV -")
	fmt.Println(hex.EncodeToString(spcContainer.AesKeyIV))
	fmt.Println("================ SPC TLLV List ================")

}

// SPCK = RSA_OAEP d([SPCK])Prv where
// [SPCK] represents the value of SPC message bytes 24-151. Prv represents the server's private key.
func decryptSPCK(pub *rsa.PublicKey, pri *rsa.PrivateKey, enSpck []byte) ([]byte, error) {
	if len(enSpck) != 128 {
		return nil, errors.New("Wrong [SPCK] length, must be 128")
	}
	// spck, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, pri, enSpck, nil)
	spck, err := crypto.OAEPDecrypt(pub, pri, enSpck)
	if err != nil {
		// create a slice for the errors
		var errstrings []string
		errstrings = append(errstrings, fmt.Errorf("decryptSPCK error:public key cann't matched").Error())
		errstrings = append(errstrings, err.Error())
		return nil, fmt.Errorf(strings.Join(errstrings, "\n"))
	}

	return spck, nil
}

// SPC payload = AES_CBCIV d([SPC data])SPCK where
// [SPC data] represents the remaining SPC message bytes beginning at byte 176 (175 + the value of
// SPC message bytes 172-175).
// IV represents the value of SPC message bytes 8-23.
func decryptSPCpayload(spcContainer *SPCContainer, spck []byte) ([]byte, error) {
	// spcPayload, err := aes.Decrypt(spck, spcContainer.AesKeyIV, spcContainer.SPCPlayload)
	spcPayload, err := crypto.AESCBCDecrypt(spck, spcContainer.AesKeyIV, spcContainer.SPCPlayload)
	return spcPayload, err
}
