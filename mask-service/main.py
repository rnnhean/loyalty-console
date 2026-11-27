"""
敏感数据识别与脱敏服务。

为什么单独一个 Python 服务，而不是在 Go 网关里写正则：
  - 识别规则会频繁迭代（新增证件类型、新增卡前缀、以后接 NER），
    Python 侧改完重启即可，不用重新编译发版；
  - 真要做实体识别时，Python 的生态比 Go 成熟得多，迁移成本低。

它必须可降级：调用方（Go 网关）在超时或失败时会退回本地规则，
所以这里**只做尽力而为**，不做重试、不做队列，响应控制在毫秒级。
"""

from __future__ import annotations

import re
from enum import Enum

from fastapi import FastAPI
from pydantic import BaseModel, Field

app = FastAPI(title="mask-service", version="1.0.0")


class Kind(str, Enum):
    """字段类型。和 Go 侧 masking.FieldKind 的取值保持一致。"""

    PHONE = "phone"
    ID_CARD = "id_card"
    EMAIL = "email"
    BANK_CARD = "bank_card"
    PERSON_NAME = "person_name"
    NONE = "none"


# 中国大陆手机号：1 开头、第二位 3-9、共 11 位
PHONE_RE = re.compile(r"^1[3-9]\d{9}$")
# 18 位身份证：17 位数字 + 数字或 X
ID_CARD_RE = re.compile(r"^\d{17}[\dXx]$")
# 15 位老身份证
ID_CARD_15_RE = re.compile(r"^\d{15}$")
EMAIL_RE = re.compile(r"^[^@\s]+@[^@\s]+\.[A-Za-z]{2,}$")
# 银行卡：16-19 位连续数字，需再过 Luhn 校验
BANK_RE = re.compile(r"^\d{16,19}$")
# 中文姓名：2-4 个汉字（启发式，误判率不低，所以只在没有显式策略时才用）
NAME_RE = re.compile(r"^[一-龥·]{2,4}$")


def luhn_ok(number: str) -> bool:
    """Luhn 校验。

    只靠长度判断银行卡会把 18 位身份证和一堆流水号也算进去，
    加上校验位能把误判压下来一个量级。
    """
    digits = [int(c) for c in number if c.isdigit()]
    if len(digits) < 12:
        return False
    total = 0
    for i, d in enumerate(reversed(digits)):
        if i % 2 == 1:
            d *= 2
            if d > 9:
                d -= 9
        total += d
    return total % 10 == 0


def detect(value: str) -> Kind:
    """识别单个值的类型。顺序有讲究：先判更严格的格式，避免宽泛规则抢先命中。"""
    s = value.strip()
    if not s:
        return Kind.NONE
    if EMAIL_RE.match(s):
        return Kind.EMAIL
    if PHONE_RE.match(s):
        return Kind.PHONE
    if ID_CARD_RE.match(s) or ID_CARD_15_RE.match(s):
        return Kind.ID_CARD
    if BANK_RE.match(s) and luhn_ok(s):
        return Kind.BANK_CARD
    if NAME_RE.match(s):
        return Kind.PERSON_NAME
    return Kind.NONE


# 每种类型的默认保留位数：(前缀, 后缀)
KEEP = {
    Kind.PHONE: (3, 4),
    Kind.ID_CARD: (4, 2),
    Kind.BANK_CARD: (4, 4),
    Kind.EMAIL: (2, 0),
    Kind.PERSON_NAME: (1, 0),
}


def mask(value: str, kind: Kind, prefix: int | None = None, suffix: int | None = None) -> str:
    """打码。

    用字符切而不是字节切——中文姓名按字节切会切出乱码。
    """
    if not value:
        return value
    if kind == Kind.NONE:
        return value
    p, s = KEEP.get(kind, (0, 0))
    if prefix is not None:
        p = prefix
    if suffix is not None:
        s = suffix
    n = len(value)
    if p + s >= n:
        # 保留位覆盖全串时退化成全掩码，否则等于原样返回（泄漏）
        return "*" * n
    return value[:p] + "*" * (n - p - s) + (value[n - s:] if s else "")


class RedactRequest(BaseModel):
    field: str = Field(default="", description="字段名，用于日志与兜底判断")
    value: str = Field(description="原始值")
    keep_prefix: int | None = None
    keep_suffix: int | None = None


class RedactResponse(BaseModel):
    masked: str
    kind: str


class DetectResponse(BaseModel):
    kind: str
    sensitive: bool


@app.get("/health")
def health() -> dict:
    """健康检查。Go 网关用它判断是否降级。"""
    return {"status": "ok", "service": "mask-service", "version": "1.0.0"}


@app.post("/redact", response_model=RedactResponse)
def redact(req: RedactRequest) -> RedactResponse:
    """识别并脱敏。字段名只在识别不出类型时作为兜底提示。"""
    kind = detect(req.value)
    if kind == Kind.NONE and req.field:
        kind = kind_from_field(req.field)
    return RedactResponse(
        masked=mask(req.value, kind, req.keep_prefix, req.keep_suffix),
        kind=kind.value,
    )


@app.post("/detect", response_model=DetectResponse)
def detect_endpoint(req: RedactRequest) -> DetectResponse:
    """只判断是否敏感，不改值。给「导出前扫描」这类场景用。"""
    kind = detect(req.value)
    return DetectResponse(kind=kind.value, sensitive=kind != Kind.NONE)


def kind_from_field(field: str) -> Kind:
    """按字段名猜类型：值本身识别不出时才用，准确率低于按值识别。"""
    f = field.lower()
    if any(k in f for k in ("phone", "mobile", "tel")):
        return Kind.PHONE
    if any(k in f for k in ("id_card", "idcard", "identity")):
        return Kind.ID_CARD
    if any(k in f for k in ("email", "mail")):
        return Kind.EMAIL
    if any(k in f for k in ("bank", "card_no")):
        return Kind.BANK_CARD
    if "name" in f:
        return Kind.PERSON_NAME
    return Kind.NONE
