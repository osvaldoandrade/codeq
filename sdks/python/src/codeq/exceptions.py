"""Custom exceptions for the CodeQ Python SDK."""

from __future__ import annotations

from typing import Any


class CodeQError(Exception):
    """Base exception for CodeQ client errors."""

    def __init__(self, message: str) -> None:
        super().__init__(message)
        self.message = message


class CodeQAPIError(CodeQError):
    """Raised when the CodeQ API returns an error response."""

    def __init__(
        self,
        message: str,
        status_code: int,
        response_body: Any = None,
    ) -> None:
        detail = f"{message}: {status_code}"
        if response_body is not None:
            detail += f" - {response_body}"
        super().__init__(detail)
        self.status_code = status_code
        self.response_body = response_body


class CodeQAuthError(CodeQError):
    """Raised when a required authentication token is missing."""


class CodeQTimeoutError(CodeQError):
    """Raised when a polling operation times out."""
