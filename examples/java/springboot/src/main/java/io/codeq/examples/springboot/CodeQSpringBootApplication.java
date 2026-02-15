package io.codeq.examples.springboot;

import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.scheduling.annotation.EnableAsync;
import org.springframework.scheduling.annotation.EnableScheduling;

/**
 * Spring Boot application demonstrating CodeQ integration.
 * 
 * This application acts as both a producer and worker:
 * - Producer: Creates tasks via REST API
 * - Worker: Polls and processes tasks in background
 */
@SpringBootApplication
@EnableAsync
@EnableScheduling
public class CodeQSpringBootApplication {

    public static void main(String[] args) {
        SpringApplication.run(CodeQSpringBootApplication.class, args);
    }
}
