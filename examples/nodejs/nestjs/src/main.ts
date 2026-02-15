import { NestFactory } from '@nestjs/core';
import { AppModule } from './app.module';
import { Logger } from '@nestjs/common';

/**
 * Bootstrap NestJS application with CodeQ integration.
 * 
 * This application demonstrates:
 * - Producer: REST API for creating tasks
 * - Worker: Background service for processing tasks
 */
async function bootstrap() {
  const app = await NestFactory.create(AppModule);
  
  // Enable CORS
  app.enableCors();
  
  // Global prefix
  app.setGlobalPrefix('api');
  
  const port = process.env.PORT || 3000;
  await app.listen(port);
  
  Logger.log(`Application is running on: http://localhost:${port}`, 'Bootstrap');
}

bootstrap();
