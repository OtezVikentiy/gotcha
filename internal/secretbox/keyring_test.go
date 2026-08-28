package secretbox

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// keyIDFixedMaster/keyIDFixedID — закреплённый вектор key-id: фиксированный
// мастер даёт фиксированные 8 hex-символов id, посчитанные по формуле §4
// спеки (hex(sha256("gotcha-secretbox-keyid\x00" ‖ sha256(master)))[:8]) один
// раз и записанные здесь константой. Будущая правка deriveKey/deriveKeyID
// обязана уронить именно этот тест, а не молча перевыпустить id для чужой
// продовой БД.
const (
	keyIDFixedMaster = "vector-master-v2-fixed-key-id-do-not-change"
	keyIDFixedID     = "92543729"
)

func TestKeyIDVector(t *testing.T) {
	r, err := NewKeyring(keyIDFixedMaster, "")
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	if got := r.CurrentID(); got != keyIDFixedID {
		t.Fatalf("CurrentID() = %q, want %q (closed key-id vector)", got, keyIDFixedID)
	}
}

func TestNewKeyringValidation(t *testing.T) {
	t.Run("пустой current — отказ", func(t *testing.T) {
		if _, err := NewKeyring("", ""); err == nil {
			t.Fatalf("NewKeyring(\"\", \"\") = nil error, want error")
		}
	})
	t.Run("previous == current по сырой строке — отказ", func(t *testing.T) {
		if _, err := NewKeyring("same-master", "same-master"); err == nil {
			t.Fatalf("NewKeyring(same, same) = nil error, want error")
		}
	})
	t.Run("валидная пара — успех", func(t *testing.T) {
		r, err := NewKeyring("current-master", "previous-master")
		if err != nil {
			t.Fatalf("NewKeyring: %v", err)
		}
		if r.CurrentID() == "" {
			t.Fatalf("CurrentID() empty for valid keyring")
		}
	})
	t.Run("пустой previous — кольцо из одного ключа", func(t *testing.T) {
		r, err := NewKeyring("current-master", "")
		if err != nil {
			t.Fatalf("NewKeyring: %v", err)
		}
		if r.CurrentID() == "" {
			t.Fatalf("CurrentID() empty for single-key keyring")
		}
	})
}

// TestPreviousID — PreviousID отдаёт id предыдущего ключа, выведенный той же
// деривацией, что и CurrentID (а не отдельной копией с иным путём вычисления),
// и пустую строку, когда предыдущего ключа нет.
func TestPreviousID(t *testing.T) {
	t.Run("предыдущий ключ есть — та же деривация, что у CurrentID", func(t *testing.T) {
		ring, err := NewKeyring("current-master-for-previd", "previous-master-for-previd")
		if err != nil {
			t.Fatalf("NewKeyring: %v", err)
		}
		asCurrent, err := NewKeyring("previous-master-for-previd", "")
		if err != nil {
			t.Fatalf("NewKeyring(previous as current): %v", err)
		}
		if got, want := ring.PreviousID(), asCurrent.CurrentID(); got != want {
			t.Fatalf("PreviousID() = %q, want %q (same derivation as CurrentID)", got, want)
		}
	})

	t.Run("предыдущего ключа нет — пустая строка", func(t *testing.T) {
		ring, err := NewKeyring("current-master-only", "")
		if err != nil {
			t.Fatalf("NewKeyring: %v", err)
		}
		if got := ring.PreviousID(); got != "" {
			t.Fatalf("PreviousID() = %q, want empty for single-key keyring", got)
		}
	})
}

func TestSealOpenRoundtripSingleKey(t *testing.T) {
	r, err := NewKeyring("roundtrip-master", "")
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	sealed, err := r.Seal("s3cr3t-token")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(sealed, v2Prefix) {
		t.Fatalf("Seal() = %q, want v2 envelope (prefix %q)", sealed, v2Prefix)
	}
	if !strings.Contains(sealed, r.CurrentID()) {
		t.Fatalf("Seal() = %q, want to contain current key id %q", sealed, r.CurrentID())
	}
	got, err := r.Open(sealed)
	if err != nil || got != "s3cr3t-token" {
		t.Fatalf("Open(sealed) = (%q,%v), want (s3cr3t-token,nil)", got, err)
	}
}

// TestOpenV1LegacyVector — v1-конверт, запечатанный старым (докольцевым)
// кодом, открывается кольцом с тем же мастером как текущим ключом. Вектор —
// константа v1FixedEnvelope из secretbox_test.go, не вызов удалённой функции.
func TestOpenV1LegacyVector(t *testing.T) {
	r, err := NewKeyring(v1FixedMaster, "")
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	got, err := r.Open(v1FixedEnvelope)
	if err != nil || got != v1FixedPlain {
		t.Fatalf("Open(v1 vector) = (%q,%v), want (%q,nil)", got, err, v1FixedPlain)
	}
}

// TestRingWithPrevOpensAllForms — кольцо с previous обязано открывать все три
// читаемые формы (v1 старым ключом, v2 старым, v2 текущим) и отказывать с
// понятной диагностикой на v2 постороннего ключа.
func TestRingWithPrevOpensAllForms(t *testing.T) {
	ring, err := NewKeyring("current-master-for-prev-test", v1FixedMaster)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}

	t.Run("v1 старым ключом", func(t *testing.T) {
		got, err := ring.Open(v1FixedEnvelope)
		if err != nil || got != v1FixedPlain {
			t.Fatalf("Open(v1) = (%q,%v), want (%q,nil)", got, err, v1FixedPlain)
		}
	})

	t.Run("v2 старым ключом", func(t *testing.T) {
		oldRing, err := NewKeyring(v1FixedMaster, "")
		if err != nil {
			t.Fatalf("NewKeyring(old): %v", err)
		}
		v2Old, err := oldRing.Seal("old-v2-secret")
		if err != nil {
			t.Fatalf("Seal(old): %v", err)
		}
		got, err := ring.Open(v2Old)
		if err != nil || got != "old-v2-secret" {
			t.Fatalf("Open(v2 old) = (%q,%v), want (old-v2-secret,nil)", got, err)
		}
	})

	t.Run("v2 текущим ключом", func(t *testing.T) {
		v2New, err := ring.Seal("new-v2-secret")
		if err != nil {
			t.Fatalf("Seal(new): %v", err)
		}
		got, err := ring.Open(v2New)
		if err != nil || got != "new-v2-secret" {
			t.Fatalf("Open(v2 new) = (%q,%v), want (new-v2-secret,nil)", got, err)
		}
	})

	t.Run("v2 чужого ключа — ErrOpen с id в сообщении", func(t *testing.T) {
		foreign, err := NewKeyring("totally-unrelated-master", "")
		if err != nil {
			t.Fatalf("NewKeyring(foreign): %v", err)
		}
		v2Foreign, err := foreign.Seal("foreign-secret")
		if err != nil {
			t.Fatalf("Seal(foreign): %v", err)
		}
		_, err = ring.Open(v2Foreign)
		if !errors.Is(err, ErrOpen) {
			t.Fatalf("Open(v2 foreign) err = %v, want ErrOpen", err)
		}
		if !strings.Contains(err.Error(), foreign.CurrentID()) {
			t.Fatalf("Open(v2 foreign) err = %q, want to contain foreign key id %q", err.Error(), foreign.CurrentID())
		}
	})
}

// TestOpenV1UnknownKeyNeitherRingMember — v1-конверт, запечатанный ключом,
// которого нет в кольце ни текущим, ни предыдущим: обе попытки openRaw
// проваливаются, Open обязан отдать ErrOpen. Отличается от
// TestOpenV1LegacyVector (там текущий ключ кольца совпадает с ключом
// вектора) — здесь не совпадает ни один из двух.
func TestOpenV1UnknownKeyNeitherRingMember(t *testing.T) {
	ring, err := NewKeyring("v1-unknown-current-master", "v1-unknown-previous-master")
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	got, err := ring.Open(v1FixedEnvelope)
	if !errors.Is(err, ErrOpen) {
		t.Fatalf("Open(v1FixedEnvelope) err = %v, want ErrOpen", err)
	}
	if got != "" {
		t.Fatalf("Open(v1FixedEnvelope) = %q, want empty on ErrOpen", got)
	}
}

// TestOpenV2BitFlippedCiphertext — v2-конверт, запечатанный ТЕКУЩИМ ключом
// кольца, у которого затем испорчен байт в теле ciphertext (id конверта
// остаётся верным). keyByID находит ключ по id без проблем, поэтому падение
// происходит именно на проверке Poly1305 внутри openRaw — сценарий, который
// старый пакет покрывал TestOpenBitFlippedCiphertext, а новые тесты Keyring
// не унаследовали (существующие «нечитаемые» кейсы используют ЧУЖОЙ id и
// бьют мимо, отказывая ещё в keyByID).
func TestOpenV2BitFlippedCiphertext(t *testing.T) {
	ring, err := NewKeyring("bitflip-current-master", "")
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	sealed, err := ring.Seal("bitflip-secret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// sealed = "enc:v2:<id>:<base64(nonce24‖ciphertext)>" — портим байт
	// строго внутри тела ciphertext (после 24-байтного nonce), а не в id и
	// не в base64-обвязке, чтобы декодирование не свалилось раньше времени
	// и падение случилось на Poly1305, а не на разборе конверта.
	prefix := v2Prefix + ring.CurrentID() + ":"
	if !strings.HasPrefix(sealed, prefix) {
		t.Fatalf("Seal() = %q, want prefix %q", sealed, prefix)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed[len(prefix):])
	if err != nil {
		t.Fatalf("DecodeString: %v", err)
	}
	if len(raw) <= 24 {
		t.Fatalf("decoded payload too short to contain ciphertext: %d bytes", len(raw))
	}
	raw[24] ^= 0x01 // флип бита в первом байте ciphertext, сразу после nonce
	corrupted := prefix + base64.StdEncoding.EncodeToString(raw)

	got, err := ring.Open(corrupted)
	if !errors.Is(err, ErrOpen) {
		t.Fatalf("Open(corrupted v2) err = %v, want ErrOpen", err)
	}
	if got != "" {
		t.Fatalf("Open(corrupted v2) = %q, want empty on ErrOpen", got)
	}
}

// TestRewrapMatrix — матрица §4 спеки целиком.
func TestRewrapMatrix(t *testing.T) {
	ring, err := NewKeyring("rewrap-current-master", v1FixedMaster)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}

	t.Run(`"" — без изменений`, func(t *testing.T) {
		out, changed, err := ring.Rewrap("")
		if err != nil || changed || out != "" {
			t.Fatalf("Rewrap(\"\") = (%q,%v,%v), want (\"\",false,nil)", out, changed, err)
		}
	})

	t.Run("plaintext — запечатать текущим", func(t *testing.T) {
		out, changed, err := ring.Rewrap("plain-secret")
		if err != nil || !changed {
			t.Fatalf("Rewrap(plaintext) = (%q,%v,%v), want (_,true,nil)", out, changed, err)
		}
		got, err := ring.Open(out)
		if err != nil || got != "plain-secret" {
			t.Fatalf("Open(rewrapped plaintext) = (%q,%v), want (plain-secret,nil)", got, err)
		}
		if !strings.Contains(out, ring.CurrentID()) {
			t.Fatalf("Rewrap(plaintext) = %q, want to contain current key id", out)
		}
	})

	t.Run("ложный enc: — считается plaintext, запечатывается", func(t *testing.T) {
		out, changed, err := ring.Rewrap("enc:not-base64!!")
		if err != nil || !changed {
			t.Fatalf("Rewrap(false enc:) = (%q,%v,%v), want (_,true,nil)", out, changed, err)
		}
		got, err := ring.Open(out)
		if err != nil || got != "enc:not-base64!!" {
			t.Fatalf("Open(rewrapped false enc:) = (%q,%v), want (enc:not-base64!!,nil)", got, err)
		}
	})

	t.Run("v2 текущего ключа — без изменений", func(t *testing.T) {
		sealed, err := ring.Seal("already-current")
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		out, changed, err := ring.Rewrap(sealed)
		if err != nil || changed || out != sealed {
			t.Fatalf("Rewrap(v2 current) = (%q,%v,%v), want (%q,false,nil)", out, changed, err, sealed)
		}
	})

	t.Run("v1 предыдущим ключом — открыть и перезапечатать", func(t *testing.T) {
		out, changed, err := ring.Rewrap(v1FixedEnvelope)
		if err != nil || !changed {
			t.Fatalf("Rewrap(v1 prev) = (%q,%v,%v), want (_,true,nil)", out, changed, err)
		}
		if !strings.HasPrefix(out, v2Prefix) || !strings.Contains(out, ring.CurrentID()) {
			t.Fatalf("Rewrap(v1 prev) = %q, want v2 envelope with current key id", out)
		}
		got, err := ring.Open(out)
		if err != nil || got != v1FixedPlain {
			t.Fatalf("Open(rewrapped v1) = (%q,%v), want (%q,nil)", got, err, v1FixedPlain)
		}
	})

	t.Run("v2 предыдущим ключом — открыть и перезапечатать", func(t *testing.T) {
		oldRing, err := NewKeyring(v1FixedMaster, "")
		if err != nil {
			t.Fatalf("NewKeyring(old): %v", err)
		}
		v2Old, err := oldRing.Seal("old-key-secret")
		if err != nil {
			t.Fatalf("Seal(old): %v", err)
		}
		out, changed, err := ring.Rewrap(v2Old)
		if err != nil || !changed {
			t.Fatalf("Rewrap(v2 prev) = (%q,%v,%v), want (_,true,nil)", out, changed, err)
		}
		got, err := ring.Open(out)
		if err != nil || got != "old-key-secret" {
			t.Fatalf("Open(rewrapped v2 prev) = (%q,%v), want (old-key-secret,nil)", got, err)
		}
	})

	t.Run("нечитаемое значение — ErrOpen, значение не тронуто", func(t *testing.T) {
		foreign, err := NewKeyring("rewrap-foreign-master", "")
		if err != nil {
			t.Fatalf("NewKeyring(foreign): %v", err)
		}
		v2Foreign, err := foreign.Seal("unreadable")
		if err != nil {
			t.Fatalf("Seal(foreign): %v", err)
		}
		out, changed, err := ring.Rewrap(v2Foreign)
		if !errors.Is(err, ErrOpen) {
			t.Fatalf("Rewrap(v2 foreign) err = %v, want ErrOpen", err)
		}
		if changed || out != v2Foreign {
			t.Fatalf("Rewrap(v2 foreign) = (%q,%v), want (%q,false) — value must stay untouched", out, changed, v2Foreign)
		}
	})
}
