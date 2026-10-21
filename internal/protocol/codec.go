package protocol

import "encoding/json"

// Codec 抽象「信封 ↔ 字节」的序列化，使业务层不感知具体编码格式。
//
// 当前唯一实现是 JSONLine（JSON + 行分隔）。未来若要换 Protobuf + 长度前缀帧，
// 只需新增一个实现并在装配处替换，core 与 adapter 代码不动。
type Codec interface {
	Encode(Envelope) ([]byte, error)
	Decode([]byte) (Envelope, error)
}

// JSONLine 是 NDJSON 编解码器：每行一个 JSON 信封，以 '\n' 结尾。
//
// framing（换行符）归属在此而非业务层：换行是 NDJSON 传输的帧边界，
// 与 JSON 序列化本身无关，放在 codec 里只是因为当前只有这一种传输形态。
type JSONLine struct{}

func (JSONLine) Encode(e Envelope) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (JSONLine) Decode(line []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(line, &e); err != nil {
		return Envelope{}, err
	}
	return e, nil
}
