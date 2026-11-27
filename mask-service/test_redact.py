"""识别与脱敏的单元测试。

跑法：python -m pytest test_redact.py -q
（依赖装在镜像里；本机没有 python 环境时用容器跑，见 README）

测试里刻意用了**假的**号码：手机号 13800001234 是运营商预留号段，
身份证 110101199001011234 的校验位是人为构造的，不对应任何真实个人。
"""

from main import Kind, detect, luhn_ok, mask


def test_detect_phone():
    assert detect("13800001234") == Kind.PHONE
    assert detect("12345678901") == Kind.NONE  # 第二位 2，不是手机号


def test_detect_id_card():
    assert detect("110101199001011234") == Kind.ID_CARD
    assert detect("11010119900101123X") == Kind.ID_CARD
    assert detect("110101900101123") == Kind.ID_CARD  # 15 位老证


def test_detect_email():
    assert detect("alice@example.com") == Kind.EMAIL
    assert detect("not-an-email") == Kind.NONE


def test_detect_bank_card_requires_luhn():
    # 4111111111111111 是公开测试卡号，Luhn 校验通过
    assert detect("4111111111111111") == Kind.BANK_CARD
    # 同样长度但校验位错误，不应被当成银行卡
    assert detect("4111111111111112") != Kind.BANK_CARD


def test_detect_name():
    assert detect("张三丰") == Kind.PERSON_NAME
    assert detect("李四") == Kind.PERSON_NAME


def test_mask_phone():
    assert mask("13800001234", Kind.PHONE) == "138****1234"


def test_mask_id_card():
    assert mask("110101199001011234", Kind.ID_CARD) == "1101************34"


def test_mask_name_keeps_one_char():
    assert mask("张三丰", Kind.PERSON_NAME) == "张**"


def test_mask_never_returns_full_value_for_short_input():
    # 保留位覆盖全串时必须全掩码，否则等于没脱敏
    out = mask("123", Kind.PHONE, prefix=3, suffix=4)
    assert out == "***"


def test_mask_passthrough_for_non_sensitive():
    assert mask("文档云 Pro", Kind.NONE) == "文档云 Pro"


def test_luhn():
    assert luhn_ok("4111111111111111")
    assert not luhn_ok("4111111111111112")
