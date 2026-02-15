package io.codeq.sdk;

/**
 * Exception thrown by CodeQ SDK operations.
 */
public class CodeQException extends Exception {
    
    public CodeQException(String message) {
        super(message);
    }

    public CodeQException(String message, Throwable cause) {
        super(message, cause);
    }
}
